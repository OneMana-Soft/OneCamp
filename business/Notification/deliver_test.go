package notification

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	MQTT "github.com/eclipse/paho.mqtt.golang"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// recordingMqtt is the broker as Deliver sees it: every publish, kept.
type recordingMqtt struct {
	MQTT.Client
	mu  sync.Mutex
	got []published
}

type published struct {
	topic   string
	payload []byte
}

func (r *recordingMqtt) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := payload.([]byte)
	r.got = append(r.got, published{topic, b})
	return doneToken{}
}

type doneToken struct{}

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (doneToken) Error() error                   { return nil }

func recordMqtt(t *testing.T) *recordingMqtt {
	t.Helper()
	r := &recordingMqtt{}
	was := mqttInit.MqttClient
	mqttInit.MqttClient = r
	t.Cleanup(func() { mqttInit.MqttClient = was })
	return r
}

// notices reads the desktop notices r saw, by the user whose topic each went to.
func (r *recordingMqtt) notices(t *testing.T, users ...string) map[string]mqttStruct.MqttDesktopNotice {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	byTopic := map[string]string{}
	for _, u := range users {
		byTopic[helpers.GetMqttTopicForUserActivity(u)] = u
	}
	out := map[string]mqttStruct.MqttDesktopNotice{}
	for _, p := range r.got {
		var msg struct {
			Type int8                         `json:"type"`
			Data mqttStruct.MqttDesktopNotice `json:"data"`
		}
		if err := json.Unmarshal(p.payload, &msg); err != nil || msg.Type != mqttStruct.MESSAGE_DESKTOP_NOTICE {
			continue
		}
		u, ok := byTopic[p.topic]
		if !ok {
			t.Fatalf("a notice went to %s, nobody's activity topic here", p.topic)
		}
		if _, twice := out[u]; twice {
			t.Fatalf("%s was sent the notice twice", u)
		}
		out[u] = msg.Data
	}
	return out
}

func watchPushes(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	t.Cleanup(firebaseInit.ObservePushesForTest(func(_ map[string]string, tokens []string) {
		mu.Lock()
		calls = append(calls, tokens)
		mu.Unlock()
	}))
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

const (
	ana = "0a0a0a0a-0000-4000-8000-000000000001"
	tom = "0a0a0a0a-0000-4000-8000-000000000002"
	pam = "0a0a0a0a-0000-4000-8000-000000000003"
)

func chatPush(body string) map[string]string {
	return map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   ana + " " + tom,
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: "m1",
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:     "Dm - Tom",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:      body,
	}
}

// Whoever the gate lets through gets one notice on their own topic, with or
// without a device; the push goes to the devices there are.
func TestDeliverNoticesEveryoneTheGateLetsThrough(t *testing.T) {
	r := recordMqtt(t)
	pushes := watchPushes(t)
	var asked []string
	reach := func(_ context.Context, ids []string) ([]string, []string, error) {
		asked = ids
		return []string{ana, tom}, []string{"tok-tom-phone", "tok-tom-laptop"}, nil // pam is paused
	}

	if err := deliver(context.Background(), []string{ana, tom, pam}, chatPush("Lunch?"), "chat:g:m1", reach); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{ana, tom, pam}) {
		t.Errorf("asked the gate about %v", asked)
	}
	got := r.notices(t, ana, tom, pam)
	if len(got) != 2 || got[ana].NoticeID != "chat:g:m1" || got[tom].NoticeID != "chat:g:m1" {
		t.Fatalf("notices %+v, want one each for ana (no device) and tom, none for pam", got)
	}
	if got[ana].Data[firebaseInit.FIREBASE_PUSH_DATA_BODY] != "Lunch?" || got[ana].Data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != "chat" {
		t.Errorf("the notice carries %v, want the push's own data", got[ana].Data)
	}
	if calls := pushes(); len(calls) != 1 || !slices.Equal(calls[0], []string{"tok-tom-phone", "tok-tom-laptop"}) {
		t.Errorf("pushes %v, want tom's two devices in one call", calls)
	}
}

func TestDeliverWithNoDeviceAnywhereStillNotices(t *testing.T) {
	r := recordMqtt(t)
	pushes := watchPushes(t)
	reach := func(context.Context, []string) ([]string, []string, error) { return []string{ana}, nil, nil }
	if err := deliver(context.Background(), []string{ana}, chatPush("Hi"), "chat:g:m2", reach); err != nil {
		t.Fatal(err)
	}
	if got := r.notices(t, ana); len(got) != 1 {
		t.Fatalf("notices %v, want ana's", got)
	}
	if calls := pushes(); len(calls) != 0 {
		t.Errorf("pushed %v with no device to push to", calls)
	}
}

// When who may be told can't be read, nobody is: not the push, not a notice.
func TestDeliverTellsNobodyWhenTheGateCantBeRead(t *testing.T) {
	r := recordMqtt(t)
	pushes := watchPushes(t)
	reach := func(context.Context, []string) ([]string, []string, error) { return nil, nil, errors.New("db down") }
	if err := deliver(context.Background(), []string{ana}, chatPush("Hi"), "chat:g:m3", reach); err == nil {
		t.Fatal("no error for a gate that couldn't be read")
	}
	if len(r.got) != 0 || len(pushes()) != 0 {
		t.Errorf("published %d, pushed %v", len(r.got), pushes())
	}
}

// The notice is the push's data, its body cut to about 1 KB at a rune, the
// whole message under noticeMax however the text escapes, and the push's own
// map left as it was.
func TestDesktopNoticeIsThePushCutToSize(t *testing.T) {
	at := time.Date(2026, 10, 13, 9, 0, 0, 7_000_000, time.UTC)
	long := strings.Repeat("ünïcødé ", 800) // ~10 KB of two-byte runes
	push := chatPush(long)
	before := maps.Clone(push)

	n := desktopNotice(push, "chat:g:m1", at)
	if !maps.Equal(push, before) {
		t.Fatal("desktopNotice changed the push's own data")
	}
	body := n.Data[firebaseInit.FIREBASE_PUSH_DATA_BODY]
	if len(body) > helpers.PushBodyMax || !utf8.ValidString(body) || !strings.HasSuffix(body, "…") {
		t.Errorf("body is %d bytes (valid UTF-8: %v), want at most %d, cut at a rune, ending …", len(body), utf8.ValidString(body), helpers.PushBodyMax)
	}
	if n.SentAt != "2026-10-13T09:00:00.007Z" || n.NoticeID != "chat:g:m1" {
		t.Errorf("sent_at %q id %q", n.SentAt, n.NoticeID)
	}
	for k, v := range before {
		if k != firebaseInit.FIREBASE_PUSH_DATA_BODY && n.Data[k] != v {
			t.Errorf("%s = %q, want the push's %q", k, n.Data[k], v)
		}
	}

	// Text that escapes ("<" is six bytes as JSON), and a huge icon link: still
	// under the ceiling, the title kept.
	heavy := chatPush(strings.Repeat("<", 1000))
	heavy[firebaseInit.FIREBASE_PUSH_DATA_ICON] = "https://files.example/" + strings.Repeat("x", 6000)
	h := desktopNotice(heavy, "chat:g:m2", at)
	if size := noticeSize(h); size > noticeMax || size == 0 {
		t.Errorf("message is %d bytes, want at most %d", size, noticeMax)
	}
	if h.Data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Dm - Tom" {
		t.Errorf("title %q, want it whole", h.Data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
	}
}
