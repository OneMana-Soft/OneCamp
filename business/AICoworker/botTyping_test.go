package aicoworker

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
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// typingBroker stands in for the MQTT broker: it keeps every message
// published, and when.
type typingBroker struct {
	MQTT.Client
	mu   sync.Mutex
	at   []time.Time
	data []map[string]any
}

func (b *typingBroker) Publish(_ string, _ byte, _ bool, payload interface{}) MQTT.Token {
	var msg struct {
		Data map[string]any `json:"data"`
	}
	body, _ := payload.([]byte)
	_ = json.Unmarshal(body, &msg)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.at = append(b.at, time.Now())
	b.data = append(b.data, msg.Data)
	return &MQTT.DummyToken{}
}

// The assistant answering in a channel or a conversation says it is typing as
// a bot of its kind, so the app says it "is working", and says so again every
// repeat until it stops, so the line never lapses while it works.
func TestAssistantTypingSaysItsKindAndKeepsTheLineUp(t *testing.T) {
	bot := &userBusiness.BotIdentity{UUID: "ai", Name: "OneCamp AI", Kind: userDomain.BotKindAssistant}
	for _, c := range []struct {
		name  string
		run   func(ctx context.Context, stop <-chan struct{})
		where string
	}{
		{"channel", func(ctx context.Context, stop <-chan struct{}) { runBotTyping(ctx, bot, "channel-1", stop) }, "channel_uuid"},
		{"conversation", func(ctx context.Context, stop <-chan struct{}) { runBotChatTyping(ctx, bot, "group-1", stop) }, "chat_grp_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker := &typingBroker{}
			was := mqttInit.MqttClient
			mqttInit.MqttClient = broker
			t.Cleanup(func() { mqttInit.MqttClient = was })

			var started, stopped time.Time
			synctest.Test(t, func(t *testing.T) {
				stop := make(chan struct{})
				done := make(chan struct{})
				started = time.Now()
				go func() {
					defer close(done)
					c.run(context.Background(), stop)
				}()
				time.Sleep(30*time.Second + 2*time.Second)
				stopped = time.Now()
				close(stop)
				<-done
			})

			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.at) < 2 || !broker.at[0].Equal(started) {
				t.Fatalf("%d messages, the first at %v: want one at once and then repeats", len(broker.at), broker.at)
			}
			for i, d := range broker.data {
				if d["is_bot"] != true || d["user_bot_kind"] != "assistant" || d["user_uuid"] != "ai" || d[c.where] == nil {
					t.Fatalf("message %d is %v, want the assistant typing as a bot of its kind", i, d)
				}
			}
			last := started
			for _, at := range append(broker.at, stopped) {
				if gap := at.Sub(last); gap > mqttBusiness.TypingRepublishInterval {
					t.Fatalf("%v passed without a message while the assistant worked", gap)
				}
				last = at
			}
		})
	}
}
