package mqttInit

import (
	"context"
	"errors"
	"testing"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
)

// sentClient keeps what was published, by topic.
type sentClient struct {
	MQTT.Client
	sent map[string]string
}

func (c *sentClient) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	c.sent[topic] = string(payload.([]byte))
	return nil
}

// On the demo, a payload for any topic the shared visitor may be subscribed
// to goes out with every address but the visitor's blanked; one person's own
// activity topic, when that person isn't the visitor, goes out as it is, and
// so does the admins' broadcast, which the visitor can never subscribe to.
func TestTheDemoBlanksAddressesWhereTheVisitorMayListen(t *testing.T) {
	t.Setenv("JWT_SECRET", "demo-client-topic-secret")
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	visitor, member, unknown := uuid.New(), uuid.New(), uuid.New()
	old := emailOf
	t.Cleanup(func() { emailOf = old })
	emailOf = func(_ context.Context, id uuid.UUID) (string, error) {
		switch id {
		case visitor:
			return "visitor@demo.example", nil
		case member:
			return "owner@example.com", nil
		}
		return "", errors.New("no such person")
	}

	payload := `{"actor":{"user_email_id":"owner@example.com"},"to":{"user_email_id":"visitor@demo.example"}}`
	blanked := `{"actor":{"user_email_id":""},"to":{"user_email_id":"visitor@demo.example"}}`
	want := map[string]string{
		helpers.GetMqttTopicForChannelMessage(uuid.NewString()): blanked, // shared: the visitor may be there
		helpers.GetMqttTopicForChannelTyping(uuid.NewString()):  blanked,
		helpers.GetMqttTopicForDoc(uuid.NewString()):            blanked,
		helpers.GetPublicUsersStatusTopic():                     blanked,
		helpers.GetMqttTopicForUserActivity(visitor.String()):   blanked, // the visitor's own
		helpers.GetMqttTopicForUserActivity(member.String()):    payload, // a member's own
		helpers.GetMqttTopicForAdminBroadcast():                 payload, // admins alone
		helpers.GetMqttTopicForUserActivity(unknown.String()):   blanked, // can't tell: blanked
	}
	sent := &sentClient{sent: map[string]string{}}
	client := demoClient{sent}
	for topic := range want {
		client.Publish(topic, 1, false, []byte(payload))
	}
	for topic, w := range want {
		if sent.sent[topic] != w {
			t.Errorf("%s: sent %s, want %s", topic, sent.sent[topic], w)
		}
	}

	// Off the demo nothing changes, wherever it goes.
	t.Setenv("DEMO_MODE", "")
	topic := helpers.GetMqttTopicForChannelMessage(uuid.NewString())
	client.Publish(topic, 1, false, []byte(payload))
	if sent.sent[topic] != payload {
		t.Errorf("off the demo: sent %s", sent.sent[topic])
	}
}
