//go:build integration

package integration_test

// A room stops saying it records when its recording ends on its own (V-630).
// The room's metadata, which every screen in the call reads, kept saying
// "Recording" with its red dot after an egress failed or was aborted by
// itself, and after a start the egress refused, until the call ended; a Stop
// then found nothing running and answered 400 without clearing it. Now the
// server clears it on an end it hears of, unless the grid takes the recording
// over, and only while the room still names that recording or nothing records
// it; and a Stop by someone allowed to stop clears a room that says it records
// with nothing running, and answers 200. Who may start or stop is unchanged.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARecordingThatEndsOnItsOwnStopsSayingItRecords(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	stageServers(t)
	failed := func(e *livekit.EgressInfo) { e.SetFailed(errors.New("out of memory")) }

	// seat seats Asha (its moderator) and Ben in a channel's call of its
	// own, with the web at web: a stage recording, or LiveKit's grid for "".
	seat := func(t *testing.T, web string) (string, map[string]*dgraphStruct.DgraphUser) {
		t.Helper()
		t.Setenv("FRONTEND_DOMAIN", web)
		room := uuid.NewString()
		people := stageChannel(t, pg, dg, room)
		enterCall(t, ctx, lk, room, people, "Asha", "Ben")
		return room, people
	}
	says := func(t *testing.T, room string, on bool, egressID string) {
		t.Helper()
		if got, _, id := roomRecording(t, ctx, room); got != on || id != egressID {
			t.Errorf("the room says recording %v as %q, want %v as %q", got, id, on, egressID)
		}
	}
	ended := func(t *testing.T, info *livekit.EgressInfo) {
		t.Helper()
		lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: info})
		settle(t)
	}

	for _, c := range []struct {
		name, web string
		end       func(*livekit.EgressInfo)
	}{
		{"a grid recording fails", "", failed},
		{"a stage recording fails after 30 s", "team.example.test", func(e *livekit.EgressInfo) {
			e.StartedAt = time.Now().Add(-40 * time.Second).UnixNano()
			failed(e)
		}},
		{"a stage recording reaches its limit before it starts", "team.example.test", func(e *livekit.EgressInfo) {
			e.SetAborted(livekit.MsgLimitReachedWithoutStart)
		}},
		{"a grid recording reaches its limit", "", func(e *livekit.EgressInfo) { e.SetLimitReached() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker := &noticeBroker{}
			integration.UseMqttClient(t, broker)
			room, people := seat(t, c.web)
			info := startRecordingOf(t, egress, room, people["Asha"])
			says(t, room, true, info.EgressId)
			settle(t)
			broker.forget()
			ended(t, egress.EndAs(t, info.EgressId, c.end))
			says(t, room, false, "")
			// The people outside the call (the channel's call banner) hear it
			// stopped too, from the end alone.
			if heard := broker.callRecording(helpers.GetMqttTopicForChannelMessage(room)); len(heard) == 0 || heard[len(heard)-1] {
				t.Errorf("the channel heard the call's recording as %v after its end, want it last heard off", heard)
			}
			// Nothing records it, and the room says so: a Stop is the
			// asker's view being behind, answered as on every route.
			if code := recordingRoute(t, false, room, people["Asha"]); code != http.StatusBadRequest {
				t.Errorf("Asha's stop after it ended: %d, want 400", code)
			}
		})
	}

	t.Run("the grid takes over, and the first's end racing itself leaves it named", func(t *testing.T) {
		room, people := seat(t, "team.example.test")
		first := startRecordingOf(t, egress, room, people["Asha"])
		end := egress.EndAs(t, first.EgressId, failed)
		// LiveKit sends a webhook again when it isn't answered in time: two
		// copies of the first's end race; one records again on the grid, and
		// the other must not clear the grid's recording off the room.
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); callControlBusiness.OnRecordingEnded(ctx, end) }()
		}
		wg.Wait()
		settle(t)
		started := egress.Started(room)
		if len(started) != 2 {
			t.Fatalf("%d recordings started, want the first and one on the grid", len(started))
		}
		grid := started[1]
		says(t, room, true, grid.EgressId)
		ended(t, egress.EndAs(t, first.EgressId, func(*livekit.EgressInfo) {}))
		says(t, room, true, grid.EgressId)
		ended(t, egress.EndAs(t, grid.EgressId, failed))
		says(t, room, false, "")
	})

	t.Run("a start the egress refused", func(t *testing.T) {
		room, people := seat(t, "")
		egress.Starting = func(*livekit.RoomCompositeEgressRequest) error { return errors.New("no egress worker can take it") }
		t.Cleanup(func() { egress.Starting = nil })
		if code := recordingRoute(t, true, room, people["Asha"]); code == http.StatusOK {
			t.Fatal("a start the egress refused answered 200")
		}
		says(t, room, false, "")
	})

	t.Run("a stop after its egress ended unheard", func(t *testing.T) {
		room, people := seat(t, "")
		info := startRecordingOf(t, egress, room, people["Asha"])
		gone := egress.EndAs(t, info.EgressId, failed)
		// Ben may not stop it: his Stop is refused as before, and changes nothing.
		if code := recordingRoute(t, false, room, people["Ben"]); code != http.StatusForbidden {
			t.Errorf("Ben, a participant, stopping it: %d, want 403", code)
		}
		says(t, room, true, info.EgressId)
		if code := recordingRoute(t, false, room, people["Asha"]); code != http.StatusOK {
			t.Errorf("Asha's stop of a room that says it records with nothing running: %d, want 200", code)
		}
		says(t, room, false, "")
		// Its end, heard late, changes nothing and records nothing again.
		ended(t, gone)
		says(t, room, false, "")
		if n := len(egress.Started(room)); n != 1 {
			t.Errorf("%d recordings started, want only the first", n)
		}
	})
}

// forget drops what the broker has been sent so far.
func (b *noticeBroker) forget() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = nil
}

// callRecording is whether each call state published on topic said the call
// records, in the order sent.
func (b *noticeBroker) callRecording(topic string) []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []bool
	for _, m := range b.got {
		var msg struct {
			Type int8                     `json:"type"`
			Data mqttStruct.MqttCallState `json:"data"`
		}
		if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_CALL_STATE {
			out = append(out, msg.Data.Recording.On)
		}
	}
	return out
}
