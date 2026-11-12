package business

import (
	"errors"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
)

// stageFailedEarly: only a stage page's recording that recorded nothing of
// the stage is recorded again on the grid. One that failed within stageGrace
// of its start (LiveKit's, or the recording's own when LiveKit gave none), or
// one LiveKit aborted because the page never said it started, however long it
// waited. Never a grid recording, one that ended well, a person's stop before
// the start, or a limit.
func TestStageFailedEarly(t *testing.T) {
	now := time.Now()
	stage := func(started time.Duration, end func(*livekit.EgressInfo)) *livekit.EgressInfo {
		e := &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: &livekit.RoomCompositeEgressRequest{CustomBaseUrl: "https://team.example.test/call-recording-layout"}}}
		if started > 0 {
			e.StartedAt = now.Add(-started).UnixNano()
		}
		end(e)
		e.EndedAt = now.UnixNano()
		return e
	}
	grid := func(end func(*livekit.EgressInfo)) *livekit.EgressInfo {
		e := &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: &livekit.RoomCompositeEgressRequest{Layout: "grid-light"}}, StartedAt: now.Add(-2 * time.Second).UnixNano()}
		end(e)
		return e
	}
	failed := func(e *livekit.EgressInfo) { e.SetFailed(errors.New("page load failed")) }
	aborted := func(msg string) func(*livekit.EgressInfo) {
		return func(e *livekit.EgressInfo) { e.SetAborted(msg) }
	}
	noEnd := stage(5*time.Second, failed)
	noEnd.EndedAt = 0

	for _, c := range []struct {
		name  string
		info  *livekit.EgressInfo
		began time.Time
		want  bool
	}{
		{"failed at once", stage(2*time.Second, failed), now.Add(-3 * time.Second), true},
		{"failed before LiveKit started it, so from the recording's own start", stage(0, failed), now.Add(-5 * time.Second), true},
		{"failed with no start anywhere", stage(0, failed), time.Time{}, false},
		{"failed with no end, so until now", noEnd, now.Add(-6 * time.Second), true},
		{"failed after 31 s", stage(31*time.Second, failed), now.Add(-32 * time.Second), false},
		{"failed long after the recording's own start", stage(0, failed), now.Add(-40 * time.Second), false},
		{"the page never said it started, after 90 s", stage(90*time.Second, aborted(livekit.MsgStartNotReceived)), now.Add(-91 * time.Second), true},
		{"the page never said it started, with no start anywhere", stage(0, aborted(livekit.MsgStartNotReceived)), time.Time{}, true},
		{"a person stopped it before it started", stage(0, aborted(livekit.MsgStoppedBeforeStarted)), now.Add(-2 * time.Second), false},
		{"the session limit came before the start", stage(0, aborted(livekit.MsgLimitReachedWithoutStart)), now.Add(-2 * time.Second), false},
		{"it reached its limit", stage(2*time.Second, func(e *livekit.EgressInfo) { e.SetLimitReached() }), now.Add(-3 * time.Second), false},
		{"it ended well", stage(2*time.Second, func(e *livekit.EgressInfo) { e.SetComplete() }), now.Add(-3 * time.Second), false},
		{"a grid recording failed at once", grid(failed), now.Add(-3 * time.Second), false},
		{"a grid recording's page never said it started", grid(aborted(livekit.MsgStartNotReceived)), now.Add(-3 * time.Second), false},
		{"no egress", nil, now, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := stageFailedEarly(c.info, c.began, now); got != c.want {
				t.Errorf("stageFailedEarly = %v, want %v (%s %q)", got, c.want, c.info.GetStatus(), c.info.GetError())
			}
		})
	}
}
