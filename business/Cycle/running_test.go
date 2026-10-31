package business

import (
	"context"
	"strings"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

func TestPace(t *testing.T) {
	cases := []struct {
		p       Progress
		elapsed float64
		want    string
	}{
		{Progress{}, 0.5, "empty"},
		{Progress{Total: 10, Done: 5}, 0.5, "on_track"},
		{Progress{Total: 10, Done: 4}, 0.5, "on_track"}, // within a tenth
		{Progress{Total: 10, Done: 3}, 0.5, "behind"},
		{Progress{Total: 4}, 0.05, "on_track"}, // a cycle just begun
		{Progress{Total: 4, Done: 4}, 1, "on_track"},
	}
	for _, c := range cases {
		if got := Pace(c.p, c.elapsed); got != c.want {
			t.Errorf("Pace(%+v, %v) = %q, want %q", c.p, c.elapsed, got, c.want)
		}
	}
}

func TestElapsedAndDaysLeft(t *testing.T) {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	c := cycleModel.Cycle{StartsAt: start, EndsAt: start.AddDate(0, 0, 14)}
	now := start.AddDate(0, 0, 7)
	if e := elapsedOf(c, now); e != 0.5 {
		t.Errorf("elapsed half way = %v", e)
	}
	if d := daysLeft(c, now); d != 7 {
		t.Errorf("days left = %d, want 7", d)
	}
	if d := daysLeft(c, now.Add(time.Hour)); d != 7 {
		t.Errorf("a part day counts as one: %d, want 7", d)
	}
	if e := elapsedOf(c, start.AddDate(0, 0, -1)); e != 0 {
		t.Errorf("elapsed before the start = %v", e)
	}
	if d := daysLeft(c, c.EndsAt.Add(time.Minute)); d != 0 {
		t.Errorf("days left after the end = %d", d)
	}
}

// The list keeps the cycles of projects the reader may view, drops an
// archived project's and one they aren't in, counts each cycle's live tasks,
// and puts the soonest to end first.
func TestRunningAcrossKeepsWhatTheReaderMayView(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	mine, other, archived, viewOnly := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cy := func(p uuid.UUID, endsIn int) cycleModel.Cycle {
		return cycleModel.Cycle{Id: uuid.New(), ProjectUUID: p, Number: 1, Name: "Cycle 1", StartsAt: now.AddDate(0, 0, -7), EndsAt: now.AddDate(0, 0, endsIn)}
	}
	cMine, cOther, cArchived, cView := cy(mine, 7), cy(other, 3), cy(archived, 2), cy(viewOnly, 1)

	saved := []any{runningCycles, cycleMembers, readProjects, standingsOf, taskStatuses}
	defer func() {
		runningCycles = saved[0].(func(context.Context, []uuid.UUID, time.Time) ([]cycleModel.Cycle, error))
		cycleMembers = saved[1].(func(context.Context, []uuid.UUID) (map[uuid.UUID][]string, error))
		readProjects = saved[2].(func(context.Context, string) ([]*dgraphStruct.DgraphProject, error))
		standingsOf = saved[3].(func(context.Context, string, []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error))
		taskStatuses = saved[4].(func(context.Context, []string) ([]*dgraphStruct.DgraphTask, error))
	}()
	runningCycles = func(_ context.Context, ids []uuid.UUID, at time.Time) ([]cycleModel.Cycle, error) {
		if !at.Equal(now) {
			t.Fatalf("asked at %v", at)
		}
		// Only the projects the reader may view are asked for.
		want := map[uuid.UUID]bool{mine: true, viewOnly: true}
		if len(ids) != len(want) {
			t.Fatalf("asked for the cycles of %v", ids)
		}
		var out []cycleModel.Cycle
		for _, id := range ids {
			if !want[id] {
				t.Fatalf("asked for the cycles of %s, which the reader can't see", id)
			}
		}
		for _, c := range []cycleModel.Cycle{cView, cMine} {
			out = append(out, c)
		}
		return out, nil
	}
	readProjects = func(_ context.Context, uid string) ([]*dgraphStruct.DgraphProject, error) {
		if uid != "0xreader" {
			t.Fatalf("read projects for %q", uid)
		}
		return []*dgraphStruct.DgraphProject{
			{Uuid: mine.String(), Name: "Launch"},
			{Uuid: other.String(), Name: "Not mine"},
			{Uuid: archived.String(), Name: "Old"},
			{Uuid: viewOnly.String(), Name: "Audit"},
		}, nil
	}
	standingsOf = func(_ context.Context, who string, _ []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error) {
		if who != "reader" {
			t.Fatalf("standings for %q", who)
		}
		return map[string]workAccess.Standing{
			strings.ToLower(mine.String()):     {ProjectUUID: mine.String(), Role: workAccess.RoleMember},
			strings.ToLower(other.String()):    {ProjectUUID: other.String(), Role: workAccess.RoleNone},
			strings.ToLower(archived.String()): {ProjectUUID: archived.String(), Role: workAccess.RoleAdmin, Archived: true},
			strings.ToLower(viewOnly.String()): {ProjectUUID: viewOnly.String(), Role: workAccess.RoleViewer},
		}, nil
	}
	cycleMembers = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
		for _, id := range ids {
			if id == cOther.Id || id == cArchived.Id {
				t.Fatalf("read the tasks of a cycle the reader can't see")
			}
		}
		return map[uuid.UUID][]string{cMine.Id: {"a", "b", "c", "gone"}, cView.Id: {"d"}}, nil
	}
	taskStatuses = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{
			{Uuid: "a", Status: "done"}, {Uuid: "b", Status: "inProgress"}, {Uuid: "c", Status: "todo"}, {Uuid: "d", Status: "todo"},
		}, nil
	}

	got, err := RunningAcross(context.Background(), "reader", "0xreader", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cycles, want the two the reader may view: %+v", len(got), got)
	}
	if got[0].ProjectName != "Audit" || got[1].ProjectName != "Launch" {
		t.Fatalf("order: %q then %q, want the soonest to end first", got[0].ProjectName, got[1].ProjectName)
	}
	l := got[1]
	if l.Progress != (Progress{Total: 3, Started: 1, Done: 1}) || l.State != "current" || l.DaysLeft != 7 || l.Elapsed != 0.5 || l.Pace != "behind" {
		t.Fatalf("Launch's cycle: %+v", l)
	}
}

func TestRunningAcrossForAReaderInNoProjectReadsNothingElse(t *testing.T) {
	saved, savedRead := runningCycles, readProjects
	defer func() { runningCycles, readProjects = saved, savedRead }()
	readProjects = func(context.Context, string) ([]*dgraphStruct.DgraphProject, error) { return nil, nil }
	runningCycles = func(context.Context, []uuid.UUID, time.Time) ([]cycleModel.Cycle, error) {
		t.Fatal("read cycles for a reader in no project")
		return nil, nil
	}
	got, err := RunningAcross(context.Background(), "reader", "0xreader", time.Now())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty list", got, err)
	}
}
