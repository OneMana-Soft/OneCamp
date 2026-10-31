package Domain

// A move of a task and the chain it pushes along (DG10), written at once:
// one Postgres transaction holding UPDATE … ANY open around one Dgraph upsert
// whose condition is that every task is still as the move read it. The graph
// commits first; Postgres commits after it, or rolls back when the graph
// didn't write. If Postgres's commit fails after the graph's, the graph write
// is undone (helpers.CompensateOnFailure): dates put back, history lines
// taken off. So no task is left moved while another isn't.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// MoveGuard is one task as the move read it, by its graph uid: its dates
// then (nil: it had none). The write goes ahead only while every task is
// still so, and live.
type MoveGuard struct {
	Uid        string
	Start, Due *time.Time
}

// ErrMoveStale is a move whose tasks changed (dates, or deleted) after it
// read them: nothing was written; read and plan again.
var ErrMoveStale = errors.New("the tasks changed since the move read them")

// moveSeams are where a move writes: seams for tests.
var moveSeams = struct {
	inTx  func(ctx context.Context, fn func(context.Context) error) error
	touch func(ctx context.Context, ids []uuid.UUID, at time.Time) error
	write func(ctx context.Context, q string, vars map[string]string, cond string, ts []*dgraphStruct.DgraphTask) (*api.Response, error)
	undo  func(ctx context.Context, restore []*dgraphStruct.DgraphTask, lines map[string][]string) error
}{
	inTx:  pgexec.InTx,
	touch: models.TouchTasks,
	write: dgraphModels.SetDgraphTasksIf,
	undo:  dgraphModels.UndoDgraphTaskDates,
}

var graphUID = regexp.MustCompile(`^0x[0-9a-f]+$`)

// CommitMove writes a move: writes are the tasks' new dates and history
// lines (each by uid), ids their Postgres rows, marked changed at at; guards
// are the tasks as read. ErrMoveStale when a guard no longer holds; any other
// error is a failed write. Either way nothing is left half done.
func CommitMove(ctx context.Context, ids []uuid.UUID, at time.Time, writes []*dgraphStruct.DgraphTask, guards []MoveGuard) error {
	q, cond, err := moveGuard(guards)
	if err != nil {
		return err
	}
	// Each new history line gets a name, so an undo can find it.
	names := map[string]string{} // line name → task uid
	for i, w := range writes {
		for j, a := range w.Activity {
			if a != nil && a.Uid == "" {
				a.Uid = fmt.Sprintf("_:h%d_%d", i, j)
				names[strings.TrimPrefix(a.Uid, "_:")] = w.Uid
			}
		}
	}
	wrote := false
	var made map[string]string
	err = moveSeams.inTx(ctx, func(tctx context.Context) error {
		if err := moveSeams.touch(tctx, ids, at); err != nil {
			return err
		}
		resp, err := moveSeams.write(ctx, q, nil, cond, writes)
		if err != nil {
			return err
		}
		if !guardHeld(resp, len(guards)) {
			return ErrMoveStale
		}
		wrote, made = true, resp.GetUids()
		return nil
	})
	if err == nil || !wrote {
		if err != nil && !errors.Is(err, ErrMoveStale) {
			helpers.LogErrorWithContext(ctx, "domain/CommitMove couldn't move %d tasks err: %+v", len(writes), err)
		}
		return err
	}
	// The graph moved and Postgres didn't commit: put the graph back.
	lines := map[string][]string{}
	for name, task := range names {
		if uid := made[name]; uid != "" {
			lines[task] = append(lines[task], uid)
		}
	}
	restore := make([]*dgraphStruct.DgraphTask, 0, len(guards))
	for _, g := range guards {
		restore = append(restore, &dgraphStruct.DgraphTask{Uid: g.Uid, StartDate: dateOrZero(g.Start), DueDate: dateOrZero(g.Due)})
	}
	_ = helpers.CompensateOnFailure(ctx, "a move's graph write", func(uctx context.Context) error {
		return moveSeams.undo(uctx, restore, lines)
	})
	return err
}

func dateOrZero(t *time.Time) *time.Time {
	if t != nil {
		return t
	}
	zero := time.Time{}
	return &zero
}

// moveGuard is the upsert's query and condition: every guarded task is live
// and has the dates it was read with.
func moveGuard(guards []MoveGuard) (string, string, error) {
	if len(guards) == 0 {
		return "", "", errors.New("domain/CommitMove: a move with no tasks")
	}
	uids := make([]string, 0, len(guards))
	conds := make([]string, 0, len(guards))
	for _, g := range guards {
		if !graphUID.MatchString(g.Uid) {
			return "", "", fmt.Errorf("domain/CommitMove: %q is not a graph uid", g.Uid)
		}
		uids = append(uids, g.Uid)
		conds = append(conds, "(uid("+g.Uid+") AND "+dateIs("task_start_date", g.Start)+" AND "+dateIs("task_due_date", g.Due)+")")
	}
	q := `{ g(func: uid(` + strings.Join(uids, ", ") + `)) @filter(` + dgraphStruct.TASK_LIVE_FILTER + ` AND (` + strings.Join(conds, " OR ") + `)) { ok as uid } }`
	return q, fmt.Sprintf("@if(eq(len(ok), %d))", len(guards)), nil
}

func dateIs(pred string, t *time.Time) string {
	if t == nil {
		return "NOT has(" + pred + ")"
	}
	return "eq(" + pred + `, "` + t.UTC().Format(time.RFC3339Nano) + `")`
}

// guardHeld is whether the upsert's guard found all n tasks as read.
func guardHeld(resp *api.Response, n int) bool {
	if resp == nil {
		return false
	}
	var out struct {
		G []struct {
			Uid string `json:"uid"`
		} `json:"g"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return false
	}
	return len(out.G) == n
}
