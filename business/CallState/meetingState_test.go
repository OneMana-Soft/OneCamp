package business

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

// heardBroker keeps what it is asked to publish, by topic.
type heardBroker struct {
	MQTT.Client
	mu   sync.Mutex
	sent map[string][]byte
}

func (b *heardBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, _ := payload.([]byte)
	b.sent[topic] = raw
	return &MQTT.DummyToken{}
}

func (b *heardBroker) topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.sent))
	for t := range b.sent {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// hearTheBroker puts a broker that listens in place for one test.
func hearTheBroker(t *testing.T) *heardBroker {
	t.Helper()
	b := &heardBroker{sent: map[string][]byte{}}
	before := mqttInit.MqttClient
	mqttInit.MqttClient = b
	t.Cleanup(func() { mqttInit.MqttClient = before })
	return b
}

// A meeting belongs to no conversation, so its call's state goes to every
// member in the call, each on their own topic: its host hears that someone
// knocked and reads the lobby again (the knock bar), as a channel's members
// hear on the channel's topic. Every member in it, not only the twelve faces
// the payload names; nobody who left, and no guest, who has no topic. The
// list of whom it went to never goes out with it. A channel's call still
// goes to the channel's topic and nowhere else.
func TestAMeetingsCallStateReachesEveryMemberInIt(t *testing.T) {
	broker := hearTheBroker(t)
	at := time.Now().Add(-time.Minute)
	const meeting = "6f1c0e2a-1111-4222-8333-944455556666"
	s := row(at)
	s.Room, s.Kind, s.ResourceKind, s.ResourceID = "meet-"+meeting, model.KindMeeting, KindMeeting, meeting
	var people []model.Attendee
	var members []string
	for i := 0; i < statePeople+2; i++ {
		id := uuid.New()
		members = append(members, helpers.GetMqttTopicForUserActivity(id.String()))
		people = append(people, model.Attendee{Identity: fmt.Sprintf("0x%d", i+1), UserUUID: &id, Name: fmt.Sprintf("P%d", i), Role: model.RoleParticipant, JoinedAt: at})
	}
	people[0].Role = model.RoleHost
	gone := uuid.New()
	left := at.Add(time.Second)
	people = append(people,
		model.Attendee{Identity: "guest-kai", Name: "Kai", Role: model.RoleGuest, JoinedAt: at},
		model.Attendee{Identity: "0x99", UserUUID: &gone, Name: "Gone", Role: model.RoleParticipant, JoinedAt: at, LeftAt: &left},
	)

	mqttBusiness.PublishCallState(statePayload(viewOf(s, people, nil), liveKitBusiness.CallView{}))

	sort.Strings(members)
	if got := broker.topics(); fmt.Sprint(got) != fmt.Sprint(members) {
		t.Fatalf("the meeting's state went to %d topics %v, want the %d members' own %v", len(got), got, len(members), members)
	}
	for _, topic := range members {
		var msg struct {
			Type int8           `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(broker.sent[topic], &msg); err != nil {
			t.Fatalf("%s heard %q: %v", topic, broker.sent[topic], err)
		}
		if msg.Type != int8(mqttStruct.MESSAGE_CALL_STATE) || msg.Data["resource_kind"] != KindMeeting || msg.Data["resource_id"] != meeting {
			t.Errorf("%s heard %+v, want the meeting's call state", topic, msg)
		}
		if _, ok := msg.Data["members"]; ok {
			t.Errorf("%s heard whom the state went to: %v", topic, msg.Data["members"])
		}
	}

	// A channel's call: its topic, and nobody's own.
	channel := hearTheBroker(t)
	maya := uuid.New()
	mqttBusiness.PublishCallState(statePayload(viewOf(row(at), []model.Attendee{{Identity: "0x1", UserUUID: &maya, Name: "Maya", Role: model.RoleHost, JoinedAt: at}}, nil), liveKitBusiness.CallView{}))
	if got := channel.topics(); len(got) != 1 || got[0] != helpers.GetMqttTopicForChannelMessage("c") {
		t.Errorf("a channel's call state went to %v, want only the channel's topic", got)
	}
}
