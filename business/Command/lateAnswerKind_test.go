package business

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// lateAnswerBroker stands in for the MQTT broker and keeps what was published.
type lateAnswerBroker struct {
	MQTT.Client
	mu   sync.Mutex
	sent [][]byte
}

func (b *lateAnswerBroker) Publish(_ string, _ byte, _ bool, payload interface{}) MQTT.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, _ := payload.([]byte)
	b.sent = append(b.sent, body)
	return &MQTT.DummyToken{}
}

// An installed app's late answer reached the app titled "Reminder", as every
// late answer did (MC-BUG-8). Each now says what it is: a fired /remind a
// reminder, an app's answer an app's.
func TestALateAnswerSaysWhatItIs(t *testing.T) {
	broker := &lateAnswerBroker{}
	was := mqttInit.MqttClient
	mqttInit.MqttClient = broker
	t.Cleanup(func() { mqttInit.MqttClient = was })

	deliverAsync(context.Background(), CommandContext{}, &commandAdapter.CommandResponse{ResponseType: "ephemeral", Text: "Created JIRA-12"})

	// A reminder for whoever set it; an id that is no one's reaches no
	// device and no saved item, so nothing but the card is sent.
	payload, _ := json.Marshal(reminderPayload{Text: "stretch", CreatedBy: "no-one"})
	if err := runReminderJob(context.Background(), &jobModel.ScheduledJob{Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, body := range broker.sent {
		var msg struct {
			Data commandAdapter.CommandResponse `json:"data"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatal(err)
		}
		got = append(got, msg.Data.Kind+": "+msg.Data.Text)
	}
	want := []string{"app: Created JIRA-12", "reminder: ⏰ Reminder: stretch"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("published %q, want %q", got, want)
	}
}
