//go:build integration
// +build integration

package integration_test

// After it loses the broker, the server's own client, connected as main
// connects it, comes back by itself and holds its subscriptions again, and
// /health/ready's realtime line says FAIL while it is away and ok once it is
// back. Three ways of losing a broker, each seen in life:
//
//   - the broker drops the client, as it does with one whose keepalive ran out
//     while its machine slept;
//   - the broker stops answering and closes nothing, which is how a sleeping
//     machine looks from the other end of a connection (the laptop running a
//     test server was suspended for two hours on 15 Oct 2026);
//   - the broker restarts, as on every update.
//
// The broker runs with the compose files' sign-in and access rules, and the
// compose file's health check for it passes against the same image.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheServerComesBackAfterTheBrokerGoes -v

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheServerComesBackAfterTheBrokerGoes(t *testing.T) {
	broker := integration.SetupOwnBroker(t)
	before := mqttInit.MqttClient
	t.Cleanup(func() {
		if mqttInit.MqttClient != nil {
			mqttInit.MqttClient.Disconnect(100)
		}
		mqttInit.MqttClient = before
	})

	clientID := "backend-reconnect-" + uuid.NewString()[:8]
	if err := mqttInit.ConnectMqtt(&mqttInit.MqttConfig{Broker: broker.MQTT, ClientID: clientID, Username: "backend", CleanSession: true}); err != nil {
		t.Fatalf("connect the server's client: %v", err)
	}
	since, _ := broker.Connection(t, clientID)
	// The client subscribes once it is connected, a moment after ConnectMqtt
	// returns.
	var held []string
	for deadline := time.Now().Add(30 * time.Second); len(held) == 0; time.Sleep(250 * time.Millisecond) {
		if held = heldSubscriptions(t, broker, clientID); len(held) == 0 && time.Now().After(deadline) {
			t.Fatalf("the server's client holds no subscriptions on its first connection; the broker says:\n%s", tail(broker.Logs(t), 40))
		}
	}
	realtimeLine(t, "ok", "once connected")

	// comesBack waits for a connection newer than the last one, holding what
	// the first one held.
	comesBack := func(t *testing.T, when string) {
		t.Helper()
		deadline := time.Now().Add(150 * time.Second)
		for {
			at, connected := broker.Connection(t, clientID)
			if connected && at != "" && at != since {
				if got := heldSubscriptions(t, broker, clientID); reflect.DeepEqual(got, held) {
					since = at
					realtimeLine(t, "ok", when)
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s the client didn't come back with its subscriptions (connected %v since %q, holding %v; want %v)",
					when, connected, at, heldSubscriptions(t, broker, clientID), held)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	t.Run("the broker drops it", func(t *testing.T) {
		broker.Kick(t, clientID)
		comesBack(t, "after the broker dropped it")
	})

	t.Run("the broker stops answering, as when the machine sleeps, and wakes", func(t *testing.T) {
		thaw := broker.Freeze(t)
		// The client pings after 30 s of quiet and gives up 10 s later.
		deadline := time.Now().Add(90 * time.Second)
		for mqttInit.MqttClient.IsConnectionOpen() {
			if time.Now().After(deadline) {
				t.Fatal("the client never noticed that the broker stopped answering")
			}
			time.Sleep(time.Second)
		}
		realtimeLine(t, "FAIL", "while the broker is frozen")
		thaw()
		comesBack(t, "after the broker woke")
	})

	t.Run("the broker restarts", func(t *testing.T) {
		broker.Restart(t)
		comesBack(t, "after the broker restarted")
	})

	t.Run("the compose file's health check passes against the broker's image", func(t *testing.T) {
		cmd := composeHealthCheck(t, "emqx")
		if code := broker.Exec(t, cmd...); code != 0 {
			t.Fatalf("%v exited %d", cmd, code)
		}
	})
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// heldSubscriptions is what the broker says the client holds, in order.
func heldSubscriptions(t *testing.T, broker *integration.BrokerEnv, clientID string) []string {
	t.Helper()
	topics := broker.Subscriptions(t, clientID)
	sort.Strings(topics)
	return topics
}

// realtimeLine asks /health/ready, as make api-ready does from inside the
// container, and wants its realtime line to start with status.
func realtimeLine(t *testing.T, status, when string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	helpers.ServeReadiness(rec, req)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.TrimSuffix(f[1], ":") == "realtime" {
			if f[0] != status {
				t.Fatalf("%s /health/ready says %q, want %s", when, line, status)
			}
			return
		}
	}
	t.Fatalf("%s /health/ready has no realtime line:\n%s", when, rec.Body.String())
}

// composeHealthCheck is a service's health check command in the shipped
// compose file, as the container runs it ("CMD" dropped).
func composeHealthCheck(t *testing.T, service string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			in = strings.TrimSpace(line) == service+":"
			continue
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "test:"); in && ok {
			var cmd []string
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &cmd); err != nil || len(cmd) < 2 || cmd[0] != "CMD" {
				t.Fatalf("%s's health check isn't a CMD list: %s", service, line)
			}
			return cmd[1:]
		}
	}
	t.Fatalf("%s has no health check in distribute-compose.yml", service)
	return nil
}
