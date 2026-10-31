package business

import (
	"context"
	"strings"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// stubPlan reads tasks for every plan and judges them as stubShift does.
func stubPlan(t *testing.T, tasks []*dgraphStruct.DgraphTask, move map[string]bool) {
	t.Helper()
	old := planSeams
	t.Cleanup(func() { planSeams = old })
	planSeams.read = planOf(func(string, domain.PlanLimits) ([]*dgraphStruct.DgraphTask, domain.PlanCut) {
		return tasks, domain.PlanCut{}
	})
	// A forecast's totals: what the graph would count of the same tasks.
	oldForecast := forecastSeams
	t.Cleanup(func() { forecastSeams = oldForecast })
	forecastSeams.totals = totalsFor(func(_ context.Context, project string, bounds []time.Time) domain.PlanTotals {
		return totalsOf(project, tasks, bounds)
	})
	stubShift(t, tasks, move)
}

func TestTheCriticalPathLeavesOutWhatTheReaderCantSee(t *testing.T) {
	loc := time.UTC
	a, b, c, d := chainOf(loc)
	stubPlan(t, []*dgraphStruct.DgraphTask{a, b, c, d}, nil)
	hidden := shiftSeams.judge
	shiftSeams.judge = func(ctx context.Context, u, uid string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		out, err := hidden(ctx, u, uid, ts...)
		out[strings.ToLower(d.Uuid)] = workAccess.Verdict{}
		return out, err
	}
	got, err := CriticalPath(context.Background(), "p", loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range got.Tasks {
		if pt.UUID == d.Uuid || pt.External {
			t.Fatalf("q's d is hidden and takes no part: %+v", got.Tasks)
		}
	}
	if len(got.Tasks) != 3 || len(got.Links) != 2 || got.Finish == nil || *got.Finish != "2026-10-10" {
		t.Fatalf("a, b and c with their two links; a ends last: %+v", got)
	}
}

func TestImpactCountsTasksProjectsAndStuck(t *testing.T) {
	loc := time.UTC
	a, b, c, d := chainOf(loc)
	stubPlan(t, []*dgraphStruct.DgraphTask{a, b, c, d}, map[string]bool{d.Uuid: false})
	due := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	got, err := Impact(context.Background(), a, TaskDates{Due: &due}, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Delays.Tasks != 2 || got.Delays.Projects != 1 || got.Stuck.Tasks != 1 || len(got.Moves) != 2 {
		t.Fatalf("b and c move in p; q's d, seen not movable, is stuck: %+v", got)
	}
	if got.Finish.Before == nil || got.Finish.After == nil || *got.Finish.Before != "2026-10-10" || *got.Finish.After != "2026-10-16" {
		t.Fatalf("p finished on the 10th; after, b runs 13-14 and c 15-16: %v %v", *got.Finish.Before, *got.Finish.After)
	}
	sub := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: "p"}}
	if got, err := Impact(context.Background(), sub, TaskDates{Due: &due}, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}); err != nil || got.Delays.Tasks != 0 {
		t.Fatalf("a sub-task pushes nothing along: %+v %v", got, err)
	}
}

// planOf is a plan read that answers each project asked with read.
func planOf(read func(project string, limits domain.PlanLimits) ([]*dgraphStruct.DgraphTask, domain.PlanCut)) func(context.Context, []string, string, domain.PlanLimits) (map[string]domain.PlanRead, error) {
	return func(_ context.Context, ids []string, _ string, limits domain.PlanLimits) (map[string]domain.PlanRead, error) {
		out := map[string]domain.PlanRead{}
		for _, id := range ids {
			tasks, cut := read(id, limits)
			out[strings.ToLower(id)] = domain.PlanRead{Tasks: tasks, Cut: cut}
		}
		return out, nil
	}
}

// totalsFor is a totals read that answers each project asked with read.
func totalsFor(read func(ctx context.Context, project string, bounds []time.Time) domain.PlanTotals) func(context.Context, []string, []time.Time) (map[string]domain.PlanTotals, error) {
	return func(ctx context.Context, ids []string, bounds []time.Time) (map[string]domain.PlanTotals, error) {
		out := map[string]domain.PlanTotals{}
		for _, id := range ids {
			out[strings.ToLower(id)] = read(ctx, id, bounds)
		}
		return out, nil
	}
}
