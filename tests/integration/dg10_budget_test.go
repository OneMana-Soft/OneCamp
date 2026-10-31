//go:build integration
// +build integration

package integration_test

// DG10's budget runners (lines in dgraph_budgets_test.go): the critical path
// and the forecast of the project holding the fixture's long chain, and a move of the chain's
// head that pushes the chain along. The move is a write: each call moves the
// head a day later than the last, so the chain moves every time.

import (
	"context"
	"fmt"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

func dg10Critical(ctx context.Context, f *dgfixture.Fixture) (int, error) {
	project, err := dg10ChainProject(ctx, f)
	if err != nil {
		return 0, err
	}
	got, err := taskBusiness.CriticalPath(ctx, project, time.UTC, dg10Viewer(f))
	if err != nil {
		return 0, err
	}
	return nonEmpty("the critical path", len(got.Tasks), nil)
}

// dg10Forecast is the forecast of the chain's project (linear-20): the plan
// read, then the graph's counts of the project's tasks.
func dg10Forecast(ctx context.Context, f *dgfixture.Fixture) (int, error) {
	project, err := dg10ChainProject(ctx, f)
	if err != nil {
		return 0, err
	}
	got, err := taskBusiness.Forecast(ctx, project, time.Now(), time.UTC, dg10Viewer(f))
	if err != nil {
		return 0, err
	}
	return nonEmpty("the forecast's open work", int(got.Open), nil)
}

func dg10Move(ctx context.Context, f *dgfixture.Fixture) (int, error) {
	if _, err := dg10ChainProject(ctx, f); err != nil {
		return 0, err
	}
	// Past every task waiting on it, a day further each call: each kind of
	// link pushes them along every time.
	dg10Moves++
	start := dg10TargetPast.AddDate(0, 0, dg10Moves)
	due := start.Add(time.Hour)
	moved, err := taskBusiness.MoveTaskDates(ctx, &dgraphStruct.DgraphTask{Uuid: dg10Target}, taskBusiness.TaskDates{Start: &start, Due: &due}, time.UTC, dg10Viewer(f))
	return nonEmpty("the tasks pushed along", len(moved), err)
}

var (
	dg10Project    string
	dg10Target     string    // the first open, dated task of the chain that an open, dated task waits on
	dg10TargetPast time.Time // the last day of the tasks waiting on it
	dg10Moves      int
)

func dg10Viewer(f *dgfixture.Fixture) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: f.ViewerUID(), Uuid: f.Viewer.UUID}
}

// dg10ChainProject is the chain's project, and the task a move moves: the
// first open, dated task of the chain that an open, dated task waits on (the
// fixture's statuses are random, and a finished task holds nothing up). Read
// once, in a warm-up, before any call is counted.
func dg10ChainProject(ctx context.Context, f *dgfixture.Fixture) (string, error) {
	if dg10Project != "" {
		return dg10Project, nil
	}
	type node struct {
		UUID    string    `json:"task_uuid"`
		Status  string    `json:"task_status"`
		Start   time.Time `json:"task_start_date"`
		Due     time.Time `json:"task_due_date"`
		Project struct {
			UUID string `json:"project_uuid"`
		} `json:"task_project"`
		Waiting []node `json:"~task_blocked_by"`
	}
	out, err := dgexec.Read[struct {
		Chain []node `json:"chain"`
	}](ctx, `query q($t: string) {
		h as var(func: eq(task_uuid, $t))
		var(func: uid(h)) @recurse(loop: false) { c as ~task_blocked_by }
		chain(func: uid(h, c)) @filter(`+dgraphStruct.TASK_LIVE_FILTER+` AND `+dgraphStruct.TASK_OPEN_FILTER+`) {
			task_uuid task_status task_start_date task_due_date task_project { project_uuid }
			~task_blocked_by @filter(`+dgraphStruct.TASK_LIVE_FILTER+` AND `+dgraphStruct.TASK_OPEN_FILTER+`) { task_uuid task_start_date task_due_date }
		}
	}`, map[string]string{"$t": f.ChainHead})
	if err != nil {
		return "", fmt.Errorf("the chain: %w", err)
	}
	for _, n := range out.Chain {
		if n.Due.Year() <= 1970 || n.Start.Year() <= 1970 || len(n.Waiting) == 0 {
			continue
		}
		past := n.Due
		for _, w := range n.Waiting {
			if w.Due.After(past) {
				past = w.Due
			}
		}
		dg10Project, dg10Target, dg10TargetPast = n.Project.UUID, n.UUID, past
		return dg10Project, nil
	}
	return "", fmt.Errorf("the chain has no open task that an open task waits on")
}
