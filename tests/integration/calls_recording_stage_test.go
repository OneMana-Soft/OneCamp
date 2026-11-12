//go:build integration

package integration_test

// The recording draws the stage (meet-17, H3). With a web origin LiveKit's
// recorder can reach, a call's recording opens the web's layout page
// (CustomBaseUrl), which draws the same stage the call does; with none (a
// no-domain install, or the web on this machine's localhost), LiveKit's own
// grid, as before. A stage page that fails in its first 30 s (the page is
// missing, or the recorder's Chrome can't reach it) is recorded again, once,
// on LiveKit's grid: the same recording goes on under the new egress, so
// nobody is shown a failed or empty one, and it is still the starter's.
//
// The recording is started through the channel's own route, the recorder is
// the fake egress service behind LiveKit's egress API, and its end reaches
// the server's own webhook handler, signed as LiveKit signs it.

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

// stageServers puts the web and the API on hosts of their own, as a domain
// install has them, with the server's secret the recording stage's tickets
// are sealed under: a call's recording then draws the stage.
func stageServers(t *testing.T) {
	t.Helper()
	t.Setenv("FE_HOST_DOMAIN", "")
	t.Setenv("BACKEND_DOMAIN", "api.example.test")
	t.Setenv("JWT_SECRET", "the integration server's secret")
}

// opensStage reports whether a recording's request opens the recording
// stage's entrance with a ticket: the address itself is never printed.
func opensStage(req *livekit.RoomCompositeEgressRequest) bool {
	ticket, ok := strings.CutPrefix(req.GetCustomBaseUrl(), stageEntrance)
	return ok && ticket != "" && !strings.ContainsAny(ticket, "/?#&") && req.GetLayout() == ""
}

// stageChannel seeds a channel in room with Asha (its moderator) and Ben in
// it, under names of the room's own: seedChannel makes new people, and an
// email belongs to one person, so each case's call has its own.
func stageChannel(t *testing.T, pg *integration.Env, dg *integration.DgraphEnv, room string) map[string]*dgraphStruct.DgraphUser {
	t.Helper()
	tag := room[:8]
	// seedChannel keeps the last name out of the channel.
	people := seedChannel(t, pg, dg, room, "Asha"+tag, "Ben"+tag, "Cy"+tag)
	return map[string]*dgraphStruct.DgraphUser{"Asha": people["Asha"+tag], "Ben": people["Ben"+tag]}
}

// startRecordingOf starts the channel call's recording as who, and answers
// the egress it started.
func startRecordingOf(t *testing.T, egress *integration.FakeEgress, room string, who *dgraphStruct.DgraphUser) *livekit.EgressInfo {
	t.Helper()
	if code := recordingRoute(t, true, room, who); code != http.StatusOK {
		t.Fatalf("starting the recording: %d", code)
	}
	started := egress.Started(room)
	if len(started) == 0 {
		t.Fatal("no recording started")
	}
	return started[len(started)-1]
}

func TestARecordingDrawsTheStageWhenTheWebHasAnOrigin(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	stageServers(t)
	for _, c := range []struct {
		name, frontend string
		stage          bool
	}{
		{"a web host", "team.example.test", true},
		{"no web host", "", false},
		{"the web on localhost", "localhost:3001", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FRONTEND_DOMAIN", c.frontend)
			room := uuid.NewString()
			people := stageChannel(t, pg, dg, room)
			enterCall(t, ctx, lk, room, people, "Asha", "Ben")
			req := startRecordingOf(t, egress, room, people["Asha"]).GetRoomComposite()
			if opensStage(req) != c.stage {
				t.Errorf("the recorder opens the recording stage %v, want %v", opensStage(req), c.stage)
			}
			if !c.stage && (req.GetLayout() != "grid-light" || req.GetCustomBaseUrl() != "") {
				t.Errorf("with no stage page the recorder draws %q, want LiveKit's grid-light", req.GetLayout())
			}
			if req.GetRoomName() != room || len(req.GetFileOutputs()) != 1 {
				t.Errorf("the request records %q to %d files, want %q to one", req.GetRoomName(), len(req.GetFileOutputs()), room)
			}
			endRecording(t, lk, egress, egress.Started(room)[0].EgressId)
			settle(t)
		})
	}
}

func TestAStagePageThatFailsEarlyIsRecordedAgainOnTheGrid(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	stageServers(t)
	failed := func(e *livekit.EgressInfo) { e.SetFailed(errors.New("page load failed")) }

	for _, c := range []struct {
		name     string
		frontend string
		end      func(*livekit.EgressInfo)
		again    bool
	}{
		{"the stage page fails at once", "team.example.test", failed, true},
		{"the stage page fails before it started", "team.example.test", func(e *livekit.EgressInfo) {
			e.StartedAt = 0
			failed(e)
		}, true},
		{"the stage recording fails after 30 s", "team.example.test", func(e *livekit.EgressInfo) {
			e.StartedAt = time.Now().Add(-40 * time.Second).UnixNano()
			failed(e)
		}, false},
		{"a grid recording fails at once", "", failed, false},
		{"the stage recording ends well", "team.example.test", func(e *livekit.EgressInfo) {
			e.SetComplete()
			e.FileResults = []*livekit.FileInfo{{Filename: e.EgressId + ".mp4", Size: 1 << 20, Duration: int64(time.Second)}}
		}, false},
		// LiveKit aborts a web egress whose page never says it started
		// (livekit.MsgStartNotReceived), after however long it waited: the
		// page recorded nothing, so the grid takes over.
		{"the stage page never says it started", "team.example.test", func(e *livekit.EgressInfo) {
			e.StartedAt = time.Now().Add(-90 * time.Second).UnixNano()
			e.SetAborted(livekit.MsgStartNotReceived)
		}, true},
		{"a grid recording never says it started", "", func(e *livekit.EgressInfo) {
			e.SetAborted(livekit.MsgStartNotReceived)
		}, false},
		// A stop before the start is a person's, and limits are limits.
		{"the stage recording is stopped before it starts", "team.example.test", func(e *livekit.EgressInfo) {
			e.StartedAt = 0
			e.SetAborted(livekit.MsgStoppedBeforeStarted)
		}, false},
		{"the stage recording reaches its limit before it starts", "team.example.test", func(e *livekit.EgressInfo) {
			e.SetAborted(livekit.MsgLimitReachedWithoutStart)
		}, false},
		{"the stage recording reaches its limit", "team.example.test", func(e *livekit.EgressInfo) {
			e.SetLimitReached()
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FRONTEND_DOMAIN", c.frontend)
			room := uuid.NewString()
			people := stageChannel(t, pg, dg, room)
			s, _ := enterCall(t, ctx, lk, room, people, "Asha", "Ben")
			first := startRecordingOf(t, egress, room, people["Asha"])
			// A web host's recording draws its stage page; none's, the grid.
			if stage := opensStage(first.GetRoomComposite()); (c.frontend != "") != stage {
				t.Errorf("the first recording opens the recording stage %v with the web at %q", stage, c.frontend)
			}

			lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.EndAs(t, first.EgressId, c.end)})
			settle(t)

			started := egress.Started(room)
			if !c.again {
				if len(started) != 1 {
					t.Fatalf("%d recordings started, want only the first", len(started))
				}
				return
			}
			if len(started) != 2 {
				t.Fatalf("%d recordings started, want the first and one on the grid", len(started))
			}
			grid := started[1]
			if req := grid.GetRoomComposite(); req.GetCustomBaseUrl() != "" || req.GetLayout() != "grid-light" {
				t.Errorf("recorded again on an address of %d characters on %q, want LiveKit's grid-light", len(req.GetCustomBaseUrl()), req.GetLayout())
			}

			// The room's metadata names the grid recording, still Asha's.
			on, by, id := roomRecording(t, ctx, room)
			if !on || id != grid.EgressId || by == "" {
				t.Errorf("the room says recording %v by %q as %q, want on as %q", on, by, id, grid.EgressId)
			}
			// The call's record lists one recording: the grid's, running, Asha's.
			sess, err := callStateBusiness.Get(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(sess.Recordings) != 1 {
				t.Fatalf("the call's record lists %d recordings, want 1: %+v", len(sess.Recordings), sess.Recordings)
			}
			r := sess.Recordings[0]
			if r.EgressID != grid.EgressId || r.EndedAt != nil || r.Status != "recording" {
				t.Errorf("the call's record lists %+v, want %s running", r, grid.EgressId)
			}
			if r.StartedBy != people["Asha"].Uuid {
				t.Errorf("the recording is %q's, want Asha's %q", r.StartedBy, people["Asha"].Uuid)
			}
			// The channel lists the grid recording, never the failed one.
			listed := channelRecordings(t, dg, room)
			if len(listed) != 1 || listed[0] != grid.EgressId {
				t.Errorf("the channel lists %v, want only %s", listed, grid.EgressId)
			}

			// A grid recording that fails is not recorded again: no loop.
			lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.EndAs(t, grid.EgressId, failed)})
			settle(t)
			if n := len(egress.Started(room)); n != 2 {
				t.Errorf("%d recordings started after the grid's failed, want 2", n)
			}
			// The first's end, sent again, starts nothing either.
			lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.EndAs(t, first.EgressId, func(*livekit.EgressInfo) {})})
			settle(t)
			if n := len(egress.Started(room)); n != 2 {
				t.Errorf("%d recordings started after the first's end came again, want 2", n)
			}
		})
	}
}

// A person's Stop is never undone. One that lands after LiveKit ended the
// stage page's egress, before its end reaches the server, finds nothing
// running and takes the recording off the room, which still said it records
// (V-630); the end that follows records nothing again.
func TestAStopBeforeAFailedStagePagesEndIsNotUndone(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	stageServers(t)
	t.Setenv("FRONTEND_DOMAIN", "team.example.test")
	room := uuid.NewString()
	people := stageChannel(t, pg, dg, room)
	enterCall(t, ctx, lk, room, people, "Asha", "Ben")
	first := startRecordingOf(t, egress, room, people["Asha"])
	if !opensStage(first.GetRoomComposite()) {
		t.Error("the recorder doesn't open the recording stage")
	}
	ended := egress.EndAs(t, first.EgressId, func(e *livekit.EgressInfo) { e.SetAborted(livekit.MsgStartNotReceived) })
	if code := recordingRoute(t, false, room, people["Asha"]); code != http.StatusOK {
		t.Fatalf("Asha's stop after the egress ended: %d, want 200 (the room said it records)", code)
	}
	if on, _, _ := roomRecording(t, ctx, room); on {
		t.Error("the room still says it records after Asha's stop")
	}
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: ended})
	settle(t)
	if n := len(egress.Started(room)); n != 1 {
		t.Errorf("%d recordings started, want only the first: Asha stopped it", n)
	}
}
