//go:build integration

package integration_test

// A recording that captured nothing says so, and isn't called a failure.
//
// LiveKit's recording page starts on the first picture or sound in its room.
// One stopped before anything came through (every camera off and every
// microphone muted, or stopped in its first seconds) is aborted, with no file:
// the demo logged three on 28 Oct, all "ended EGRESS_ABORTED: Start signal not
// received". Each was listed and played as a failure ("This recording
// couldn't be saved"), and one that never started could be listed for
// decades, its length counted from no start at all.
//
// The recording is started and stopped through the channel's own routes, as
// its host's screen does; the recorder is the fake egress service, ended as
// LiveKit's egress ends one (its own SetAborted, SetComplete and SetFailed);
// and the end reaches the server's own webhook handler, signed as LiveKit
// signs it. What people see is read back through the channel's own routes.

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestARecordingThatCapturedNothingSaysSo(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	room := uuid.NewString()
	// Cy, named last, is kept out of the channel (seedChannel).
	people := seedChannel(t, pg, dg, room, "Asha", "Ben", "Cy")
	s, _ := enterCall(t, ctx, lk, room, people, "Asha", "Ben")
	ben := &userModels.UserInfo{UserDgraphInfo: *people["Ben"]}
	cy := &userModels.UserInfo{UserDgraphInfo: *people["Cy"]}
	var last string

	for _, c := range []struct {
		shape string
		end   func(*livekit.EgressInfo)
		want  string
	}{
		// The shape the demo logged (EGRESS_ABORTED, "Start signal not
		// received"): nothing came through to start on. LiveKit gave it a start
		// time, so its length was counted from it.
		{"aborted with nothing to start on", func(e *livekit.EgressInfo) {
			e.SetAborted(livekit.MsgStartNotReceived)
		}, "empty"},
		// Stopped before its page started, with no start time.
		{"aborted before it started", func(e *livekit.EgressInfo) {
			e.StartedAt = 0
			e.SetAborted(livekit.MsgStoppedBeforeStarted)
		}, "empty"},
		// Not seen live: an egress that says complete with nothing written.
		{"complete with nothing written", func(e *livekit.EgressInfo) {
			e.SetComplete()
			e.FileResults = []*livekit.FileInfo{{Filename: e.EgressId + ".mp4"}}
		}, "empty"},
		// A fault is still one, and one with no start time isn't decades long.
		{"failed before it started", func(e *livekit.EgressInfo) {
			e.StartedAt = 0
			e.SetFailed(errors.New("could not upload the file"))
		}, "failed"},
	} {
		t.Run(c.shape, func(t *testing.T) {
			if code := recordingRoute(t, true, room, people["Asha"]); code != http.StatusOK {
				t.Fatalf("Asha starting the recording: %d", code)
			}
			started := egress.Started(room)
			id := started[len(started)-1].EgressId
			if code := recordingRoute(t, false, room, people["Asha"]); code != http.StatusOK {
				t.Fatalf("Asha stopping the recording: %d", code)
			}
			lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.EndAs(t, id, c.end)})
			settle(t)
			last = id

			// The player, opened from the channel by a member.
			got := call(t, userController.GetChannelRecordingURL, http.MethodGet, "/ch/getRecordingURL/"+room+"/"+id, ben, "",
				map[string]string{"channel_uuid": room, "egress_id": id})
			var played map[string]any
			if err := json.Unmarshal(got.Body.Bytes(), &played); err != nil {
				t.Fatalf("the player's answer: %v (%s)", err, got.Body.String())
			}
			if got.Code != http.StatusOK || played["status"] != c.want {
				t.Errorf("the player is told %d %v, want status %q", got.Code, played, c.want)
			}
			if _, link := played["url"]; link {
				t.Errorf("the player is given a link to nothing: %v", played)
			}

			// The channel's recordings.
			listed := call(t, channelController.GetAllChannelRecordingList, http.MethodGet, "/ch/getRecordingList/"+room, ben, "",
				map[string]string{"channel_uuid": room})
			var page struct {
				Info struct {
					Recordings []struct {
						Egress  string  `json:"recording_egress_id"`
						Status  string  `json:"recording_status"`
						Seconds float64 `json:"recording_duration"`
						Size    int64   `json:"recording_size"`
					} `json:"recordings"`
				} `json:"channel_info"`
			}
			if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || listed.Code != http.StatusOK {
				t.Fatalf("the channel's recordings: %d %v", listed.Code, err)
			}
			found := false
			for _, r := range page.Info.Recordings {
				if r.Egress != id {
					continue
				}
				found = true
				if r.Status != c.want {
					t.Errorf("listed as %q, want %q", r.Status, c.want)
				}
				if r.Seconds != 0 || r.Size != 0 {
					t.Errorf("listed %v s and %d bytes long, want nothing: it holds nothing", r.Seconds, r.Size)
				}
			}
			if !found {
				t.Fatalf("the channel doesn't list %s", id)
			}

			// The call's record.
			sess, err := callStateBusiness.Get(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range sess.Recordings {
				if r.EgressID == id && r.Status != c.want {
					t.Errorf("the call's record says %q, want %q", r.Status, c.want)
				}
			}
		})
	}

	// Someone outside the channel is told nothing about it.
	if got := call(t, userController.GetChannelRecordingURL, http.MethodGet, "/ch/getRecordingURL/"+room+"/"+last, cy, "",
		map[string]string{"channel_uuid": room, "egress_id": last}); got.Code != http.StatusForbidden {
		t.Errorf("Cy, outside the channel, opening its recording: %d %s, want 403", got.Code, got.Body.String())
	}
	if got := call(t, channelController.GetAllChannelRecordingList, http.MethodGet, "/ch/getRecordingList/"+room, cy, "",
		map[string]string{"channel_uuid": room}); got.Code != http.StatusForbidden {
		t.Errorf("Cy, outside the channel, listing its recordings: %d, want 403", got.Code)
	}
}
