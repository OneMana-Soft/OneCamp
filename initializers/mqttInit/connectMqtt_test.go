package mqttInit

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// Two processes must never connect as one client: the broker drops the older
// connection whenever the other arrives.
func TestEachProcessConnectsAsItsOwnClient(t *testing.T) {
	ids := map[string]string{}
	for _, p := range []struct {
		name string
		role helpers.ServiceRole
		host string
	}{
		{"go-service", helpers.RoleAll, "3f2a1b9c0d4e"},
		{"go-service as api", helpers.RoleAPI, "3f2a1b9c0d4e"},
		{"a worker", helpers.RoleWorker, "77aa01bc2d3e"},
		{"another worker", helpers.RoleWorker, "88bb12cd3e4f"},
	} {
		id := clientIDFor("backend", p.role, p.host)
		if other, dup := ids[id]; dup {
			t.Errorf("%s and %s both connect as %q", p.name, other, id)
		}
		ids[id] = p.name
		if !strings.HasPrefix(id, "backend-") {
			t.Errorf("%s connects as %q, which doesn't say what it is", p.name, id)
		}
		if again := clientIDFor("backend", p.role, p.host); again != id {
			t.Errorf("%s: the id changed between connections of one process: %q, %q", p.name, id, again)
		}
	}
	if got := clientIDFor("", helpers.RoleWorker, "77aa01bc2d3e"); got != "" {
		t.Errorf("with no configured id the broker should name the client, got %q", got)
	}
	if got := ProcessClientID("backend", helpers.RoleWorker); !strings.HasPrefix(got, "backend-worker-") || got == "backend-worker-" {
		t.Errorf("ProcessClientID = %q", got)
	}
}

type sysEvent struct{ payload string }

func (m sysEvent) Duplicate() bool   { return false }
func (m sysEvent) Qos() byte         { return 0 }
func (m sysEvent) Retained() bool    { return false }
func (m sysEvent) Topic() string     { return "$SYS/brokers/emqx@127.0.0.1/clients/x/connected" }
func (m sysEvent) MessageID() uint16 { return 0 }
func (m sysEvent) Payload() []byte   { return []byte(m.payload) }
func (m sysEvent) Ack()              {}

// The broker announces every client, not only people's. A username without
// "_" (another backend process connects as "backend") was split and indexed,
// which panicked and ended the process. Nothing that isn't a person's client
// may reach the device count; there's no graph here, so one that did would
// panic too.
func TestClientEventsThatAreNotAPersonAreIgnored(t *testing.T) {
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	for _, payload := range []string{
		`{"username":"backend","clientid":"backend-worker-77aa01bc2d3e"}`,
		`{"username":"dashboard"}`,
		`{"username":""}`,
		`{"username":null}`,
		`{}`,
		`{"username":"user_"}`,
		`{"username":"user_not-an-id"}`,
		`{"username":"user_a_b"}`,
		`not json`,
	} {
		for name, handle := range map[string]MQTT.MessageHandler{
			"connected": connectMessageHandler, "disconnected": disconnectMessageHandler,
		} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s %s: %v", name, payload, r)
					}
				}()
				handle(nil, sysEvent{payload})
			}()
		}
	}

	// The control: a person's client does reach the device count, which with
	// no graph here panics. Without this the test above could pass by the
	// handlers ignoring everything.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a person's connect never reached the device count")
			}
		}()
		connectMessageHandler(nil, sysEvent{`{"username":"user_6f9619ff-8b86-4011-b42d-00cf4fc964ff"}`})
	}()
}

type subscribeToken struct{}

func (subscribeToken) Wait() bool                     { return true }
func (subscribeToken) WaitTimeout(time.Duration) bool { return true }
func (subscribeToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (subscribeToken) Error() error                   { return nil }

type recordingClient struct {
	MQTT.Client
	topics []string
}

func (c *recordingClient) Subscribe(topic string, _ byte, _ MQTT.MessageHandler) MQTT.Token {
	c.topics = append(c.topics, topic)
	return subscribeToken{}
}

// Every connection subscribes (it's the connect handler), and to a shared
// subscription, so one backend process, not each, takes each announcement.
func TestClientEventsAreSharedAmongBackendProcesses(t *testing.T) {
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	c := &recordingClient{}
	subscribeClientEvents(c)
	want := []string{
		"$share/backend/$SYS/brokers/+/clients/+/connected",
		"$share/backend/$SYS/brokers/+/clients/+/disconnected",
	}
	if strings.Join(c.topics, " ") != strings.Join(want, " ") {
		t.Errorf("subscribed to %v, want %v", c.topics, want)
	}
}
