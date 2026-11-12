package business

// One "cleared" per person per pass (V-577; CHAT-LEAD-35's ruling on the
// store-loop guard): the nudges of theirs that closed go out in ONE event,
// named in nudge_ids with the open count after, not one publish per nudge in a
// loop. With no ids it is the count alone (Clear all: 0).

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

// clearedRecorder keeps every payload published, by topic, in order.
type clearedRecorder struct {
	MQTT.Client
	mu   sync.Mutex
	sent map[string][][]byte
}

func (r *clearedRecorder) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := payload.([]byte)
	r.sent[topic] = append(r.sent[topic], raw)
	return &MQTT.DummyToken{}
}

type clearedEvent struct {
	Action    string   `json:"action"`
	NudgeIDs  []string `json:"nudge_ids"`
	OpenCount int      `json:"open_count"`
}

func (r *clearedRecorder) toldTo(t *testing.T, user uuid.UUID) []clearedEvent {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []clearedEvent
	for _, raw := range r.sent[helpers.GetMqttTopicForUserActivity(user.String())] {
		var m struct {
			Type int8         `json:"type"`
			Data clearedEvent `json:"data"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != mqttStruct.MESSAGE_AI_NUDGE {
			t.Fatalf("told type %d; want MESSAGE_AI_NUDGE", m.Type)
		}
		out = append(out, m.Data)
	}
	return out
}

func TestTwoClosedNudgesAreToldInOneCleared(t *testing.T) {
	rec := &clearedRecorder{sent: map[string][][]byte{}}
	before := mqttInit.MqttClient
	mqttInit.MqttClient = rec
	t.Cleanup(func() { mqttInit.MqttClient = before })

	owner, maya := uuid.New(), uuid.New()
	a, b := uuid.New(), uuid.New()
	PublishNudgeCleared(owner.String(), 1, a, b)
	got := rec.toldTo(t, owner)
	want := []string{a.String(), b.String()}
	if len(got) != 1 || got[0].Action != "cleared" || !slices.Equal(got[0].NudgeIDs, want) || got[0].OpenCount != 1 {
		t.Errorf("the owner was told %+v; want ONE cleared naming %v, with 1 open", got, want)
	}
	if told := rec.toldTo(t, maya); len(told) != 0 {
		t.Errorf("maya was told %+v; want nothing: none of hers closed", told)
	}

	PublishNudgeCleared(maya.String(), 0)
	if got := rec.toldTo(t, maya); len(got) != 1 || got[0].Action != "cleared" || len(got[0].NudgeIDs) != 0 || got[0].OpenCount != 0 {
		t.Errorf("Clear all told maya %+v; want one cleared with no ids and 0 open", got)
	}
}
