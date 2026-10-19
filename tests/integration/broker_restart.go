//go:build integration
// +build integration

package integration

// A broker of the test's own, which it may drop clients from, freeze and
// restart; SetupBroker's is for tests that leave the broker be. It is
// configured as the compose files configure it: the server's client signs in
// with a JWT made from JWT_SECRET, and the shipped emqx/acl.conf decides what
// it may subscribe to. Its ports are bound for the container's life, so the
// address a client was given still reaches it after a restart.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ownBrokerWait bounds a start of the test's broker: the first, and each
// restart. It bounds a stuck broker, not a slow one; on a busy machine EMQX
// has taken over a minute to boot.
const ownBrokerWait = 5 * time.Minute

// SetupOwnBroker starts EMQX 6.0.0 with the compose files' sign-in and access
// rules, for this test alone, and connects nothing: the test connects the
// server's client itself, as main does. It sets JWT_SECRET for the test, so
// the client's password is one the broker takes.
func SetupOwnBroker(t *testing.T) *BrokerEnv {
	t.Helper()
	guardTestState(t)
	ensureLoggers()
	ctx := context.Background()
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	password, secret := "test-"+hex.EncodeToString(raw[:12]), hex.EncodeToString(raw[12:])
	t.Setenv("JWT_SECRET", secret)
	acl, err := filepath.Abs("../../emqx/acl.conf")
	if err != nil {
		t.Fatal(err)
	}
	mqttPort, apiPort := freeLoopbackPort(t), freeLoopbackPort(t)

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "emqx/emqx:6.0.0",
			ExposedPorts: []string{"1883/tcp", "18083/tcp"},
			Env: map[string]string{
				"EMQX_DASHBOARD__DEFAULT_USERNAME":       "console",
				"EMQX_DASHBOARD__DEFAULT_PASSWORD":       password,
				"EMQX_AUTHENTICATION__1__ENABLE":         "true",
				"EMQX_AUTHENTICATION__1__SECRET":         secret,
				"EMQX_AUTHENTICATION__1__ALGORITHM":      "hmac-based",
				"EMQX_AUTHENTICATION__1__MECHANISM":      "jwt",
				"EMQX_AUTHENTICATION__1__USE_JWKS":       "false",
				"EMQX_AUTHENTICATION__1__VERIFY_CLAIMS":  `{username: "${username}"}`,
				"EMQX_AUTHORIZATION__SOURCES__1__TYPE":   "file",
				"EMQX_AUTHORIZATION__SOURCES__1__ENABLE": "true",
				"EMQX_AUTHORIZATION__SOURCES__1__PATH":   "/opt/emqx/etc/acl.conf",
				"EMQX_AUTHORIZATION__NO_MATCH":           "deny",
				"EMQX_AUTHORIZATION__CACHE__ENABLE":      "false",
			},
			Files: []testcontainers.ContainerFile{{HostFilePath: acl, ContainerFilePath: "/opt/emqx/etc/acl.conf", FileMode: 0o644}},
			HostConfigModifier: func(hc *container.HostConfig) {
				loopback := netip.MustParseAddr("127.0.0.1")
				hc.PortBindings = network.PortMap{
					network.MustParsePort("1883/tcp"):  {{HostIP: loopback, HostPort: mqttPort}},
					network.MustParsePort("18083/tcp"): {{HostIP: loopback, HostPort: apiPort}},
				}
			},
			WaitingFor: wait.ForListeningPort("1883/tcp").WithStartupTimeout(ownBrokerWait),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start the broker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(stop)
	})
	// As every Setup does: the work the test handed off settles before the
	// broker it may still be using goes (settle.go).
	settleBeforeTeardown(t)
	env := &BrokerEnv{container: c, password: password}
	env.MQTT = "tcp://127.0.0.1:" + mqttPort
	env.API = "http://127.0.0.1:" + apiPort
	env.consoleTakesLogin(t)
	return env
}

// freeLoopbackPort is a port nothing on this machine listens on right now.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// consoleTakesLogin waits until the broker's console takes a login, which it
// does a moment after the broker is up, and starts the test's login afresh.
func (e *BrokerEnv) consoleTakesLogin(t *testing.T) {
	t.Helper()
	login, _ := json.Marshal(map[string]string{"username": "console", "password": e.password})
	deadline := time.Now().Add(ownBrokerWait)
	for {
		res, err := http.Post(e.API+"/api/v5/login", "application/json", strings.NewReader(string(login)))
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				e.token = ""
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker's console never took a login: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Kick has the broker drop a client, as it does with one whose keepalive ran
// out.
func (e *BrokerEnv) Kick(t *testing.T, clientID string) {
	t.Helper()
	e.admin(t, http.MethodDelete, "/api/v5/clients/"+url.PathEscape(clientID), nil)
}

// Connection is when the broker's connection with a client began, and
// whether the client is connected now; an empty time for a client the broker
// doesn't know.
func (e *BrokerEnv) Connection(t *testing.T, clientID string) (connectedAt string, connected bool) {
	t.Helper()
	var info struct {
		ConnectedAt string `json:"connected_at"`
		Connected   bool   `json:"connected"`
	}
	e.admin(t, http.MethodGet, "/api/v5/clients/"+url.PathEscape(clientID), &info)
	return info.ConnectedAt, info.Connected
}

// Freeze stops the broker's processes where they stand and leaves its
// connections open, so nothing on its side answers and nothing says why:
// what a client sees of a machine that went to sleep. The function it
// returns thaws it; so does the end of the test.
func (e *BrokerEnv) Freeze(t *testing.T) (thaw func()) {
	t.Helper()
	ctx := context.Background()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	id := e.container.GetContainerID()
	if _, err := cli.ContainerPause(ctx, id, client.ContainerPauseOptions{}); err != nil {
		cli.Close()
		t.Fatalf("freeze the broker: %v", err)
	}
	var once sync.Once
	thaw = func() {
		once.Do(func() {
			defer cli.Close()
			if _, err := cli.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{}); err != nil {
				t.Errorf("thaw the broker: %v", err)
			}
		})
	}
	t.Cleanup(thaw)
	return thaw
}

// Restart stops the broker and starts it again at the same address, and
// waits until its console takes a login.
func (e *BrokerEnv) Restart(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	grace := 10 * time.Second
	if err := e.container.Stop(ctx, &grace); err != nil {
		t.Fatalf("stop the broker: %v", err)
	}
	if err := e.container.Start(ctx); err != nil {
		t.Fatalf("start the broker again: %v", err)
	}
	e.consoleTakesLogin(t)
}

// Exec runs a command in the broker's container and returns its exit code.
func (e *BrokerEnv) Exec(t *testing.T, cmd ...string) int {
	t.Helper()
	code, _, err := e.container.Exec(context.Background(), cmd)
	if err != nil {
		t.Fatalf("run %v in the broker: %v", cmd, err)
	}
	return code
}

// Logs is what the broker has written to its console, for a failure's
// message.
func (e *BrokerEnv) Logs(t *testing.T) string {
	t.Helper()
	r, err := e.container.Logs(context.Background())
	if err != nil {
		return "(no logs: " + err.Error() + ")"
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}
