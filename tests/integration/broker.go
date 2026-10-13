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
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
)

// BrokerEnv is a live EMQX for one test.
type BrokerEnv struct {
	container testcontainers.Container
	// MQTT is the broker's TCP address (tcp://host:port), API its admin
	// API's base (http://host:port).
	MQTT, API string
	password  string
	token     string // the test's own console login
}

// SetupBroker starts EMQX 6.0.0 and points the server at it: its own client
// (mqttInit.MqttClient), and the admin API's address and console login
// (EMQX_API_URL, EMQX_DASHBOARD_DEFAULT_USERNAME and _PASSWORD).
func SetupBroker(t *testing.T) *BrokerEnv {
	t.Helper()
	ensureLoggers()
	ctx := context.Background()
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
			WaitingFor: wait.ForAll(
				wait.ForHTTP("/api/v5/status").WithPort("18083/tcp"),
				wait.ForListeningPort("1883/tcp"),
			).WithDeadline(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start the broker: %v", err)
	}
	env := &BrokerEnv{container: c, password: password}
	t.Cleanup(env.close)

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("broker host: %v", err)
	}
	mqttPort, err := c.MappedPort(ctx, "1883")
	if err != nil {
		t.Fatalf("broker port: %v", err)
	}
	apiPort, err := c.MappedPort(ctx, "18083")
	if err != nil {
		t.Fatalf("broker admin port: %v", err)
	}
	env.MQTT = fmt.Sprintf("tcp://%s:%s", host, mqttPort.Port())
	env.API = fmt.Sprintf("http://%s:%s", host, apiPort.Port())
	// The console's account is made a moment after the broker says it's up,
	// and a login before then is refused: wait for the first one to work.
	env.waitForConsole(t)

	t.Setenv("EMQX_API_URL", env.API)
	t.Setenv("EMQX_DASHBOARD_DEFAULT_USERNAME", "console")
	t.Setenv("EMQX_DASHBOARD_DEFAULT_PASSWORD", password)

	backend := MQTT.NewClient(MQTT.NewClientOptions().AddBroker(env.MQTT).
		SetClientID("backend-test-" + hex.EncodeToString(raw[:4])).SetUsername("backend").SetAutoReconnect(false))
	if tok := backend.Connect(); !tok.WaitTimeout(30*time.Second) || tok.Error() != nil {
		t.Fatalf("connect the server's own client: %v", tok.Error())
	}
	mqttInit.MqttClient = backend
	t.Cleanup(func() {
		mqttInit.MqttClient = nil
		backend.Disconnect(100)
	})
	return env
}

func (e *BrokerEnv) waitForConsole(t *testing.T) {
	t.Helper()
	login, _ := json.Marshal(map[string]string{"username": "console", "password": e.password})
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := http.Post(e.API+"/api/v5/login", "application/json", strings.NewReader(string(login)))
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker's console login never worked: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *BrokerEnv) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.container.Terminate(ctx)
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
