package business

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// appTypingExpiry is how long the app keeps a typing line up after the last
// message about it (onecamp-fe lib/typingTiming.ts, TYPING_EXPIRY_MS).
const appTypingExpiry = 6 * time.Second

// typingBroker stands in for the MQTT broker: it keeps every message
// published, and when.
type typingBroker struct {
	MQTT.Client
	mu   sync.Mutex
	sent []brokerMessage
}

type brokerMessage struct {
	at    time.Time
	topic string
	body  []byte
}

func (b *typingBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, _ := payload.([]byte)
	b.sent = append(b.sent, brokerMessage{at: time.Now(), topic: topic, body: body})
	return &MQTT.DummyToken{}
}

// An @mentioned agent's typing line names it as an agent, so the app says
// "Release Captain is working", and while the agent works the line is said
// again often enough that the app never drops it between two messages, even
// one that arrives late.
func TestAgentTypingSaysAgentAndNeverLapses(t *testing.T) {
	broker := &typingBroker{}
	was := mqttInit.MqttClient
	mqttInit.MqttClient = broker
	t.Cleanup(func() { mqttInit.MqttClient = was })

	var started, stopped time.Time
	synctest.Test(t, func(t *testing.T) {
		bot := &userBusiness.BotIdentity{UUID: "rc", Name: "Release Captain", ProfileKey: "rc.png", Kind: userDomain.BotKindAgent}
		stop := make(chan struct{})
		done := make(chan struct{})
		started = time.Now()
		go func() {
			defer close(done)
			runAgentTyping(context.Background(), "Release Captain", bot, "channel-1", stop)
		}()
		// A run of a minute and a half that ends between two repeats.
		time.Sleep(90*time.Second + 1500*time.Millisecond)
		stopped = time.Now()
		close(stop)
		<-done
	})

	broker.mu.Lock()
	sent := broker.sent
	broker.mu.Unlock()
	if len(sent) < 2 {
		t.Fatalf("%d typing messages in a minute and a half", len(sent))
	}
	want := mqttStruct.MqttChannelTyping{UserUUID: "rc", UserName: "Release Captain", UserProfile: "rc.png", ChannelUuid: "channel-1", IsBot: true, BotKind: "agent"}
	for i, m := range sent {
		if kind, id, ok := helpers.ParseMqttTopic(m.topic); !ok || kind != helpers.MqttKindTyping || id != "channel-1" {
			t.Fatalf("message %d went to %s %s, want the channel's typing topic", i, kind, id)
		}
		var msg struct {
			Type int8                         `json:"type"`
			Data mqttStruct.MqttChannelTyping `json:"data"`
		}
		if err := json.Unmarshal(m.body, &msg); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if msg.Type != mqttStruct.MESSAGE_POST_TYPING || msg.Data != want {
			t.Fatalf("message %d is %s, want channel typing as %+v", i, m.body, want)
		}
	}

	// The first message goes at once, and from then until the run ends no
	// more than one repeat passes without one.
	if !sent[0].at.Equal(started) {
		t.Fatalf("first message %v after the run started, want at once", sent[0].at.Sub(started))
	}
	last := started
	for i, m := range sent {
		if gap := m.at.Sub(last); gap > mqttBusiness.TypingRepublishInterval {
			t.Fatalf("%v passed without a message before message %d", gap, i)
		}
		last = m.at
	}
	if gap := stopped.Sub(last); gap > mqttBusiness.TypingRepublishInterval {
		t.Fatalf("%v passed without a message before the run ended", gap)
	}
	// So the line stays up between messages even when one arrives up to a
	// repeat late: two repeats fit inside the app's expiry. At 4 s each way
	// the line blinked out between messages.
	if 2*mqttBusiness.TypingRepublishInterval > appTypingExpiry {
		t.Fatalf("repeated every %v against an expiry of %v: a message arriving late lets the line lapse", mqttBusiness.TypingRepublishInterval, appTypingExpiry)
	}
}
