package business

import (
	"testing"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// A recording is the running call's when it is filed under the call's key or
// a breakout room's, or began during the call (Kept's window); one begun
// before the call is a finished call's; one with no start time can't be told
// apart, so it is the running call's (unsure is no).
func TestARecordingIsTheRunningCallsByItsKeyOrItsStart(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	running := []callStateModel.Session{{Room: "c1", RoomSID: "RM_call", StartedAt: start},
		{Room: "c1~b1", RoomSID: "RM_breakout", StartedAt: start.Add(time.Minute)}}
	before := start.Add(-recordingDomain.CallWindowSlack - time.Minute)
	during := start.Add(10 * time.Minute)
	for _, c := range []struct {
		name  string
		key   string
		began *time.Time
		want  bool
	}{
		{"the call's key, begun before", helpers.CallSessionKeyFor("RM_call"), &before, true},
		{"a breakout room's key", helpers.CallSessionKeyFor("RM_breakout"), &before, true},
		{"an egress begun during it", "EG_1", &during, true},
		{"an egress begun before it", "EG_1", &before, false},
		{"no start time", "EG_1", nil, true},
	} {
		if got := madeBy(running, c.key, c.began); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
