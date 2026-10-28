package business

import (
	"context"
	"errors"
	"testing"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
)

// The repair takes leavers' settings away only while the conversations index
// isn't marked dirty: a failed put-back can leave it saying a member left.
func TestTheRepairTakesLeaversSettingsOnlyWhileTheIndexIsntDirty(t *testing.T) {
	prevState, prevForget := indexState, forgetLeaversNow
	t.Cleanup(func() { indexState, forgetLeaversNow = prevState, prevForget })
	at := time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		state   string
		readErr error
		forgets bool
		wantErr bool
	}{
		{name: "no comparison yet", state: "", forgets: true},
		{name: "clean", state: conversationDomain.State{Kind: conversationDomain.StateClean, At: at}.String(), forgets: true},
		{name: "drift repaired", state: conversationDomain.State{Kind: conversationDomain.StateDrift, At: at, Note: "1 repaired"}.String(), forgets: true},
		{name: "dirty", state: conversationDomain.State{Kind: conversationDomain.StateDirty, At: at, Note: "putting someone back in group g"}.String()},
		{name: "a state that can't be read", readErr: errors.New("down"), wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			called := false
			indexState = func(context.Context) (string, error) { return c.state, c.readErr }
			forgetLeaversNow = func(context.Context) (int64, error) { called = true; return 2, nil }
			n, err := forgetLeavers(context.Background())
			if called != c.forgets || (err != nil) != c.wantErr {
				t.Errorf("took leavers' settings away: %v (%d, %v), want %v (error %v)", called, n, err, c.forgets, c.wantErr)
			}
		})
	}
}
