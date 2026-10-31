package Domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

func TestTheMoveGuardHoldsEveryTaskAsRead(t *testing.T) {
	start := time.Date(2026, 11, 2, 9, 0, 0, 123, time.FixedZone("IST", 19800))
	q, cond, err := moveGuard([]MoveGuard{{Uid: "0x1", Start: &start}, {Uid: "0x2a"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"g(func: uid(0x1, 0x2a))",
		dgraphStruct.TASK_LIVE_FILTER,
		`(uid(0x1) AND eq(task_start_date, "2026-11-02T03:30:00.000000123Z") AND NOT has(task_due_date))`,
		`(uid(0x2a) AND NOT has(task_start_date) AND NOT has(task_due_date))`,
		"ok as uid",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the guard lacks %q: %s", want, q)
		}
	}
	if cond != "@if(eq(len(ok), 2))" {
		t.Errorf("the write waits on both: %s", cond)
	}
	for _, bad := range []string{"", "_:x", "0x1) OR has(task_uuid", "uid(x)"} {
		if _, _, err := moveGuard([]MoveGuard{{Uid: bad}}); err == nil {
			t.Errorf("%q is no graph uid and must be refused", bad)
		}
	}
}

// fakeMove stands in for Postgres and the graph: commit says whether
// Postgres's commit works; held whether the guard found every task.
func fakeMove(t *testing.T, touchErr, commitErr error, held bool) (wrote *int, undone *[]*dgraphStruct.DgraphTask, lines *map[string][]string) {
	t.Helper()
	old := moveSeams
	t.Cleanup(func() { moveSeams = old })
	n := 0
	var restore []*dgraphStruct.DgraphTask
	var ls map[string][]string
	moveSeams.inTx = func(ctx context.Context, fn func(context.Context) error) error {
		if err := fn(ctx); err != nil {
			return err
		}
		return commitErr
	}
	moveSeams.touch = func(context.Context, []uuid.UUID, time.Time) error { return touchErr }
	moveSeams.write = func(_ context.Context, q string, _ map[string]string, _ string, ts []*dgraphStruct.DgraphTask) (*api.Response, error) {
		n++
		found := `{"g":[{"uid":"0x1"},{"uid":"0x2"}]}`
		if !held {
			found = `{"g":[{"uid":"0x1"}]}`
		}
		uids := map[string]string{}
		for _, w := range ts {
			for _, a := range w.Activity {
				uids[strings.TrimPrefix(a.Uid, "_:")] = "0xa" + w.Uid[2:]
			}
		}
		return &api.Response{Json: []byte(found), Uids: uids}, nil
	}
	moveSeams.undo = func(_ context.Context, r []*dgraphStruct.DgraphTask, l map[string][]string) error {
		restore, ls = r, l
		return nil
	}
	return &n, &restore, &ls
}

func moveOf() ([]uuid.UUID, []*dgraphStruct.DgraphTask, []MoveGuard) {
	before := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	after := before.AddDate(0, 0, 3)
	writes := []*dgraphStruct.DgraphTask{
		{Uid: "0x1", StartDate: &after, Activity: []*dgraphStruct.DgraphTaskActivity{{Type: "start"}}},
		{Uid: "0x2", StartDate: &after, Activity: []*dgraphStruct.DgraphTaskActivity{{Type: "start"}}},
	}
	return []uuid.UUID{uuid.New(), uuid.New()}, writes, []MoveGuard{{Uid: "0x1", Start: &before}, {Uid: "0x2"}}
}

func TestAMoveCommitsWhole(t *testing.T) {
	wrote, undone, _ := fakeMove(t, nil, nil, true)
	ids, writes, guards := moveOf()
	if err := CommitMove(context.Background(), ids, time.Now(), writes, guards); err != nil || *wrote != 1 || *undone != nil {
		t.Fatalf("one write, no undo: %v %d %v", err, *wrote, *undone)
	}
}

func TestAStaleMoveWritesNothing(t *testing.T) {
	_, undone, _ := fakeMove(t, nil, nil, false)
	ids, writes, guards := moveOf()
	if err := CommitMove(context.Background(), ids, time.Now(), writes, guards); !errors.Is(err, ErrMoveStale) || *undone != nil {
		t.Fatalf("stale, and nothing to undo (the guard stopped the write): %v %v", err, *undone)
	}
}

func TestAPostgresFailureBeforeTheGraphWritesNothing(t *testing.T) {
	wrote, undone, _ := fakeMove(t, errors.New("pg down"), nil, true)
	ids, writes, guards := moveOf()
	if err := CommitMove(context.Background(), ids, time.Now(), writes, guards); err == nil || *wrote != 0 || *undone != nil {
		t.Fatalf("the graph is never written: %v %d", err, *wrote)
	}
}

// Postgres's commit fails after the graph's: the graph is put back, dates
// and history lines, so nothing is left half moved.
func TestAPostgresCommitFailureUndoesTheGraph(t *testing.T) {
	_, undone, lines := fakeMove(t, nil, errors.New("commit failed"), true)
	ids, writes, guards := moveOf()
	err := CommitMove(context.Background(), ids, time.Now(), writes, guards)
	if err == nil {
		t.Fatal("the move failed and must say so")
	}
	if len(*undone) != 2 || !(*undone)[0].StartDate.Equal(*guards[0].Start) || (*undone)[1].StartDate == nil || !(*undone)[1].StartDate.IsZero() {
		t.Fatalf("each task's dates as read are put back (none: the zero time): %+v", *undone)
	}
	if len((*lines)["0x1"]) != 1 || len((*lines)["0x2"]) != 1 {
		t.Fatalf("each task's new history line is taken off: %v", *lines)
	}
}
