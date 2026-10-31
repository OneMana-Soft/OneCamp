package business

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// A board on the stage of a call someone is in now gives them its pictures,
// and lets them add one while they draw (calls package B), as the call says
// (CallBoard). Never on a deleted board, never through an API token, and
// their own rights answer first.
func TestABoardOnACallsStageGivesItsPictures(t *testing.T) {
	oldCall, oldLive := CallBoard, boardLive
	t.Cleanup(func() { CallBoard, boardLive = oldCall, oldLive })
	var asked int
	call := map[string][2]bool{"shown": {true, false}, "drawn": {true, true}}
	CallBoard = func(_ context.Context, _, board string) (bool, bool, error) {
		asked++
		if board == "broken" {
			return false, false, errGraph
		}
		c := call[board]
		return c[0], c[1], nil
	}
	boardLive = func(_ context.Context, board string) (bool, error) { return board != "deleted", nil }
	own := func(_ context.Context, _, board string) (bool, error) { return board == "own", nil }
	reads, adds := onStage(own, false), onStage(own, true)
	ctx := context.Background()

	for _, c := range []struct {
		board      string
		read, edit bool
		err        error
	}{
		{"own", true, true, nil},
		{"elsewhere", false, false, nil},
		{"shown", true, false, nil},
		{"drawn", true, true, nil},
		{"broken", false, false, errGraph},
	} {
		r, err := reads(ctx, me, c.board)
		e, err2 := adds(ctx, me, c.board)
		if r != c.read || e != c.edit || !errors.Is(err, c.err) || !errors.Is(err2, c.err) {
			t.Errorf("%s: read %v edit %v (%v, %v); want %v %v", c.board, r, e, err, err2, c.read, c.edit)
		}
	}

	call["deleted"] = [2]bool{true, true}
	if r, _ := reads(ctx, me, "deleted"); r {
		t.Error("a deleted board's pictures through a call")
	}
	asked = 0
	token := helpers.WithActsThroughToken(ctx)
	if r, _ := reads(token, me, "drawn"); r || asked != 0 {
		t.Errorf("through an API token: read %v, the call asked %d times", r, asked)
	}
	if r, _ := reads(ctx, me, "own"); !r || asked != 0 {
		t.Error("their own rights didn't answer first")
	}
}
