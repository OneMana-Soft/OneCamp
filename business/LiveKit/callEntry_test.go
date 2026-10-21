package business

import (
	"testing"
	"time"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// A lock keeps out only the people who weren't in the call before it was
// locked, and never the people running it: someone whose network dropped can
// come back, and so can a host.
func TestALockKeepsOutOnlyNewcomers(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	locked := &callStateModel.Session{LockedAt: &at}
	before := at.Add(-time.Minute)
	after := at.Add(time.Minute)
	for _, c := range []struct {
		name string
		s    *callStateModel.Session
		a    *callStateModel.Attendee
		want bool
	}{
		{"an open call", &callStateModel.Session{}, nil, false},
		{"no call at all", nil, nil, false},
		{"someone never in it", locked, nil, true},
		{"someone in it before the lock", locked, &callStateModel.Attendee{Role: callStateModel.RoleParticipant, JoinedAt: before}, false},
		{"someone who came after the lock", locked, &callStateModel.Attendee{Role: callStateModel.RoleParticipant, JoinedAt: after}, true},
		{"a guest who came after the lock", locked, &callStateModel.Attendee{Role: callStateModel.RoleGuest, JoinedAt: after}, true},
		{"a host who came after the lock", locked, &callStateModel.Attendee{Role: callStateModel.RoleHost, JoinedAt: after}, false},
		{"a co-host who came after the lock", locked, &callStateModel.Attendee{Role: callStateModel.RoleCohost, JoinedAt: after}, false},
	} {
		if got := lockedOut(c.s, c.a); got != c.want {
			t.Errorf("%s: locked out %v, want %v", c.name, got, c.want)
		}
	}
}

// The refusals a join endpoint answers carry the code the green room acts on
// and words that say what to do.
func TestJoinRefusalsCarryTheirCode(t *testing.T) {
	if ErrRemovedFromCall.Code != "removed_from_call" || ErrRemovedFromCall.Msg != RemovedFromCallMsg {
		t.Errorf("removed: %+v", ErrRemovedFromCall)
	}
	if ErrCallLocked.Code != "call_locked" || ErrCallLocked.Msg != CallLockedMsg {
		t.Errorf("locked: %+v", ErrCallLocked)
	}
}
