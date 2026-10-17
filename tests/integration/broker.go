//go:build integration
// +build integration

package integration

// A real broker, because what's tested is the broker's own state: whether a
// client still holds a subscription, and whether its session survived. EMQX
// at the version the compose files run, with a console login, the server's
// own client connected to it as main connects it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/testcontainers/testcontainers-go"

	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
)

// BrokerEnv is the test binary's EMQX, as one test sees it.
type BrokerEnv struct {
	container testcontainers.Container
	// MQTT is the broker's TCP address (tcp://host:port), API its admin
	// API's base (http://host:port).
	MQTT, API string
	password  string
	token     string // the test's own console login
}

// brokerReadyWait is how long a broker gets to let a client in and take a
// console login after it starts. It bounds a stuck broker, not a slow one:
// at a load of about 100 a boot took from 15 s to over a minute, once a run.
const brokerReadyWait = 5 * time.Minute

// brokerConnectTry bounds one CONNECT, as the server's own connections are
// bounded (mqttInit.ConnectWithBackoff).
const brokerConnectTry = 10 * time.Second

// SetupBroker points the server at the test binary's EMQX, at the version
// the compose files run: its own client (mqttInit.MqttClient), and the admin
// API's address and console login (EMQX_API_URL,
// EMQX_DASHBOARD_DEFAULT_USERNAME and _PASSWORD).
//
// ONE BROKER A RUN. At a load of about 100 EMQX took from 15 s to over a
// minute to boot, and a broker for each test booted four a run, each adding to
// the load the others booted under. The wait was also shorter than it said:
// the 120 s deadline held two waits with a 60 s timeout each of their own, so
// a boot past a minute failed before the test's code ran
// (TestLiveJoiningAChannelTellsTheNewMembersApp, twice on 15 Oct). Now the
// broker boots once, on first use, and is ready when it lets a client in: a
// real CONNECT and CONNACK, made the way the server makes its own
// (mqttInit.ConnectWithBackoff). Tests share it without meeting: every client
// id carries the test's own uuids, and what a test does at the broker reaches
// only its own clients and topics. The container goes when the binary exits
// (testcontainers' reaper).
func SetupBroker(t *testing.T) *BrokerEnv {
	t.Helper()
	ensureLoggers()
	shared, err := theBroker()
	if err != nil {
		t.Fatalf("start the broker: %v", err)
	}
	env := *shared // the test's own copy: its console login is its own
	t.Setenv("EMQX_API_URL", env.API)
	t.Setenv("EMQX_DASHBOARD_DEFAULT_USERNAME", "console")
	t.Setenv("EMQX_DASHBOARD_DEFAULT_PASSWORD", env.password)

	backend := newBrokerClient(env.MQTT, "backend-test-")
	ctx, cancel := context.WithTimeout(context.Background(), brokerReadyWait)
	defer cancel()
	if err := mqttInit.ConnectWithBackoff(ctx, backend, mqttInit.CurrentConnectTimings()); err != nil {
		t.Fatalf("connect the server's own client: %v", err)
	}
	t.Cleanup(func() { backend.Disconnect(100) })
	UseMqttClient(t, backend)
	return &env
}

// newBrokerClient is a client of the server's kind for the broker at addr,
// with an id of its own.
func newBrokerClient(addr, idPrefix string) MQTT.Client {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	return MQTT.NewClient(MQTT.NewClientOptions().AddBroker(addr).
		SetClientID(idPrefix + hex.EncodeToString(raw)).SetUsername("backend").
		SetAutoReconnect(false).SetConnectTimeout(brokerConnectTry))
}

var theOneBroker struct {
	mu  sync.Mutex
	env *BrokerEnv
}

// theBroker is the binary's broker, started on first use. A start that failed
// leaves none, and the next test that asks starts one again.
func theBroker() (*BrokerEnv, error) {
	theOneBroker.mu.Lock()
	defer theOneBroker.mu.Unlock()
	if theOneBroker.env == nil {
		env, err := startBroker()
		if err != nil {
			return nil, err
		}
		theOneBroker.env = env
	}
	return theOneBroker.env, nil
}

// startBroker starts EMQX and waits until it lets a client in and its console
// takes a login. Nothing else is waited on: the status page and an open port
// come before either, and say less.
func startBroker() (*BrokerEnv, error) {
	ctx, cancel := context.WithTimeout(context.Background(), brokerReadyWait)
	defer cancel()
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	password := "test-" + hex.EncodeToString(raw)

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "emqx/emqx:6.0.0",
			ExposedPorts: []string{"1883/tcp", "18083/tcp"},
			Env: map[string]string{
				"EMQX_DASHBOARD__DEFAULT_USERNAME": "console",
				"EMQX_DASHBOARD__DEFAULT_PASSWORD": password,
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}
	env := &BrokerEnv{container: c, password: password}
	ready := func() error {
		host, err := c.Host(ctx)
		if err != nil {
			return fmt.Errorf("broker host: %w", err)
		}
		mqttPort, err := c.MappedPort(ctx, "1883")
		if err != nil {
			return fmt.Errorf("broker port: %w", err)
		}
		apiPort, err := c.MappedPort(ctx, "18083")
		if err != nil {
			return fmt.Errorf("broker admin port: %w", err)
		}
		env.MQTT = fmt.Sprintf("tcp://%s:%s", host, mqttPort.Port())
		env.API = fmt.Sprintf("http://%s:%s", host, apiPort.Port())
		probe := newBrokerClient(env.MQTT, "ready-probe-")
		if err := mqttInit.ConnectWithBackoff(ctx, probe, mqttInit.CurrentConnectTimings()); err != nil {
			return fmt.Errorf("the broker never let a client in: %w", err)
		}
		probe.Disconnect(0)
		// The console's account is made a moment after the broker is up, and
		// a login before then is refused: wait for the first one to work.
		return env.waitForConsole(ctx)
	}
	if err := ready(); err != nil {
		stop, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelStop()
		_ = c.Terminate(stop)
		return nil, err
	}
	return env, nil
}

// waitForConsole waits until the console takes a login, or ctx ends.
func (e *BrokerEnv) waitForConsole(ctx context.Context) error {
	login, _ := json.Marshal(map[string]string{"username": "console", "password": e.password})
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.API+"/api/v5/login", strings.NewReader(string(login)))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the broker's console login never worked: %v", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// BrokerClient is a person's client, as the web app connects one: a client
// id of its own, the person's username, and a session kept when it drops.
type BrokerClient struct {
	ID   string
	user string
	env  *BrokerEnv
	c    MQTT.Client
	got  chan BrokerMessage
	lost chan struct{}
}

// BrokerMessage is a message a client received.
type BrokerMessage struct {
	Topic   string
	Payload []byte
}

// Connect connects a person's client. present says whether the broker had a
// session for its id to resume.
func (e *BrokerEnv) Connect(t *testing.T, clientID, userUUID string) (client *BrokerClient, present bool) {
	t.Helper()
	bc := &BrokerClient{ID: clientID, user: userUUID, env: e, got: make(chan BrokerMessage, 64), lost: make(chan struct{}, 1)}
	opts := MQTT.NewClientOptions().AddBroker(e.MQTT).SetClientID(clientID).SetUsername("user_" + userUUID).
		SetCleanSession(false).SetAutoReconnect(false).SetConnectRetry(false).
		SetConnectionLostHandler(func(MQTT.Client, error) {
			select {
			case bc.lost <- struct{}{}:
			default:
			}
		}).
		SetDefaultPublishHandler(func(_ MQTT.Client, m MQTT.Message) {
			bc.got <- BrokerMessage{Topic: m.Topic(), Payload: append([]byte(nil), m.Payload()...)}
		})
	bc.c = MQTT.NewClient(opts)
	tok := bc.c.Connect()
	if !tok.WaitTimeout(30*time.Second) || tok.Error() != nil {
		t.Fatalf("connect %s: %v", clientID, tok.Error())
	}
	t.Cleanup(func() { bc.c.Disconnect(50) })
	return bc, tok.(*MQTT.ConnectToken).SessionPresent()
}

// Subscribe subscribes the client to topics, as the broker allows them.
func (c *BrokerClient) Subscribe(t *testing.T, topics ...string) {
	t.Helper()
	filters := map[string]byte{}
	for _, topic := range topics {
		filters[topic] = 1
	}
	tok := c.c.SubscribeMultiple(filters, nil)
	if !tok.WaitTimeout(15*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", c.ID, tok.Error())
	}
}

// Disconnect drops the client, keeping its session at the broker.
func (c *BrokerClient) Disconnect() { c.c.Disconnect(50) }

// Next is the next message the client receives within wait, if any.
func (c *BrokerClient) Next(wait time.Duration) (BrokerMessage, bool) {
	select {
	case m := <-c.got:
		return m, true
	case <-time.After(wait):
		return BrokerMessage{}, false
	}
}

// Lost reports whether the broker dropped the client within wait.
func (c *BrokerClient) Lost(wait time.Duration) bool {
	select {
	case <-c.lost:
		return true
	case <-time.After(wait):
		return false
	}
}

// Subscriptions is what the broker says the client holds, connected or not.
func (e *BrokerEnv) Subscriptions(t *testing.T, clientID string) []string {
	t.Helper()
	var subs []struct {
		Topic string `json:"topic"`
	}
	e.admin(t, http.MethodGet, "/api/v5/clients/"+url.PathEscape(clientID)+"/subscriptions", &subs)
	topics := make([]string, 0, len(subs))
	for _, s := range subs {
		topics = append(topics, s.Topic)
	}
	return topics
}

// admin calls the broker's admin API as the test, with its own login (kept,
// and made again when the broker no longer takes it).
func (e *BrokerEnv) admin(t *testing.T, method, path string, out any) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		if e.token == "" {
			e.token = e.login(t)
		}
		req, _ := http.NewRequest(method, e.API+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("broker %s %s: %v", method, path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusUnauthorized && attempt == 0:
			e.token = ""
			continue
		case res.StatusCode == http.StatusNotFound:
			return
		case res.StatusCode/100 != 2:
			t.Fatalf("broker %s %s: %d %s", method, path, res.StatusCode, body)
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				t.Fatalf("broker %s %s answer: %v", method, path, err)
			}
		}
		return
	}
}

func (e *BrokerEnv) login(t *testing.T) string {
	t.Helper()
	login, _ := json.Marshal(map[string]string{"username": "console", "password": e.password})
	res, err := http.Post(e.API+"/api/v5/login", "application/json", strings.NewReader(string(login)))
	if err != nil {
		t.Fatalf("broker login: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var tok struct {
		Token string `json:"token"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &tok) != nil || tok.Token == "" {
		t.Fatalf("broker login: %d %s", res.StatusCode, body)
	}
	return tok.Token
}
