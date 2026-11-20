package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// boardWorld adds Dana's boards to the world: two of hers, one that can't
// move. Their counts are read once for everyone, as the docs' are.
func boardWorld(t *testing.T, w *world) map[string][]string {
	t.Helper()
	boards := map[string][]string{dana.String(): {"b1", "b2"}}
	prev := []any{boardsOwned, boardCounts, recoverOne}
	t.Cleanup(func() {
		boardsOwned = prev[0].(func(context.Context, string, int) ([]string, int, error))
		boardCounts = prev[1].(func(context.Context, []uuid.UUID) (map[string]int, error))
		recoverOne = prev[2].(func(context.Context, accessBusiness.Subject, string, string, string) error)
	})
	boardsOwned = func(_ context.Context, owner string, first int) ([]string, int, error) {
		return append([]string(nil), boards[owner]...), len(boards[owner]), nil
	}
	boardCounts = func(_ context.Context, who []uuid.UUID) (map[string]int, error) {
		out := map[string]int{}
		for _, id := range who {
			if n := len(boards[id.String()]); n > 0 {
				out[id.String()] = n
			}
		}
		return out, nil
	}
	docs := recoverOne
	recoverOne = func(ctx context.Context, admin accessBusiness.Subject, kind, board, to string) error {
		if kind == accessBusiness.KindDoc {
			return docs(ctx, admin, kind, board, to)
		}
		if w.broken[board] {
			return errors.New("the graph refused")
		}
		for owner, bs := range boards {
			for i, b := range bs {
				if b == board {
					boards[owner] = append(bs[:i:i], bs[i+1:]...)
					boards[to] = append(boards[to], board)
				}
			}
		}
		return nil
	}
	return boards
}

// A handover gives the boards of someone who left to the heir with their docs
// (miro-32, docs C6-2): each by the one recovery an admin's takes, and one
// that can't move is counted and left for the next run.
func TestAHandoverMovesTheirBoardsWithTheirDocs(t *testing.T) {
	w := newWorld(t)
	boards := boardWorld(t, w)
	m, err := HandOver(context.Background(), admin, dana, maya)
	if err != nil || m.Docs != 3 || m.Boards != 2 || m.Failed != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	if len(boards[dana.String()]) != 0 || len(boards[maya.String()]) != 2 {
		t.Fatalf("boards %v", boards)
	}
	w2 := newWorld(t)
	boards2 := boardWorld(t, w2)
	w2.broken["b2"] = true
	if m, err := HandOver(context.Background(), admin, dana, maya); err != nil || m.Boards != 1 || m.Failed != 1 || len(boards2[dana.String()]) != 1 {
		t.Fatalf("%+v %v %v", m, err, boards2)
	}
}

// Someone who left with boards alone is still someone an admin is told
// about, and the notice names their boards.
func TestAdminsSeeWhoLeftWithBoards(t *testing.T) {
	w := newWorld(t)
	boardWorld(t, w)[quiet.String()] = []string{"q1"}
	people, err := List(context.Background())
	if err != nil || len(people) != 2 {
		t.Fatalf("%v %+v", err, people)
	}
	for _, p := range people {
		if (p.UserUUID == dana.String() && (p.Boards != 2 || p.Docs != 3)) || (p.UserUUID == quiet.String() && (p.Boards != 1 || p.Docs != 0)) {
			t.Errorf("%+v", p)
		}
	}
	if got := Work(3, 2, 2); got != "3 docs, 2 boards and 2 links" {
		t.Errorf("Work: %q", got)
	}
	if got := Work(0, 1, 0); got != "1 board" {
		t.Errorf("Work: %q", got)
	}
}
