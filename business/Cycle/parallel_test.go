package business

// Parallel cycles (jira-04 head C): every reader of "the running cycle" is
// a reader of the running cycles, tried here with two that share days in one
// project. Readers, by a grep for every caller (RunningIn, State "current",
// openCycles, runningCycle, CyclesICS): RunningAcross, the backlog plan,
// startedJoins, the switcher's read (List's states), the feed. MCP and the AI
// tools read no cycles on beta; a task's own cycle (CycleOf, the task panel's
// include) is one cycle still, as a task is in at most one.

import (
	"context"
	"strings"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// sideBySide is two cycles of one project that both run at now, and the next.
func sideBySide(project uuid.UUID, now time.Time) (web, app, next cycleModel.Cycle) {
	web = cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 1, Name: "Web", StartsAt: now.AddDate(0, 0, -3), EndsAt: now.AddDate(0, 0, 11)}
	app = cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 2, Name: "App", StartsAt: now.AddDate(0, 0, -1), EndsAt: now.AddDate(0, 0, 6)}
	next = cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 3, StartsAt: now.AddDate(0, 0, 11), EndsAt: now.AddDate(0, 0, 25)}
	return web, app, next
}

func TestTheSwitchersReadShowsBothRunningCyclesAsCurrent(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	web, app, next := sideBySide(uuid.New(), now)
	if State(web, now) != "current" || State(app, now) != "current" || State(next, now) != "upcoming" {
		t.Fatalf("both side-by-side cycles are current: %s %s %s", State(web, now), State(app, now), State(next, now))
	}
}

func TestAStartedTaskJoinsNeitherOfTwoRunningCycles(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	project, task := uuid.New(), uuid.New()
	web, app, next := sideBySide(project, now)
	on := &cycleModel.Schedule{ProjectUUID: project, AddStarted: true}
	w := &joinsWorld{schedule: on, cycles: []cycleModel.Cycle{web, app, next}, in: map[uuid.UUID]*cycleModel.Cycle{}, put: map[uuid.UUID]uuid.UUID{}}
	w.install(t)
	if err := joinRunningCycle(context.Background(), project, task, now); err != nil {
		t.Fatal(err)
	}
	if got, put := w.put[task]; put {
		t.Fatalf("joined %s; with two cycles running which one is the person's call", got)
	}
	// One running: it still joins that one.
	w.cycles = []cycleModel.Cycle{web, next}
	if err := joinRunningCycle(context.Background(), project, task, now); err != nil {
		t.Fatal(err)
	}
	if got := w.put[task]; got != web.Id {
		t.Fatalf("with one cycle running it joins it: %s", got)
	}
}

func TestTheBacklogPlanKeepsEveryRunningCycle(t *testing.T) {
	now := at(10)
	project := uuid.New()
	web, app, next := sideBySide(project, now)
	n4 := cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 4, StartsAt: now.AddDate(0, 0, 25), EndsAt: now.AddDate(0, 0, 39)}
	n5 := cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 5, StartsAt: now.AddDate(0, 0, 39), EndsAt: now.AddDate(0, 0, 53)}
	// Made before the cycles running now, numbered first.
	early := cycleModel.Cycle{Id: uuid.New(), ProjectUUID: project, Number: 0, StartsAt: now.AddDate(0, 0, 60), EndsAt: now.AddDate(0, 0, 74)}
	b := Assemble([]cycleModel.Cycle{n5, next, early, app, web, n4}, nil, &projectDomain.BacklogRead{}, now, nil)
	var got []int
	for _, c := range b.Cycles {
		got = append(got, c.Number)
	}
	if len(got) != 4 || got[0] != 0 || got[1] != 1 || got[2] != 2 || got[3] != 3 {
		t.Fatalf("both running cycles and the next two open ones, by number: %v", got)
	}
	// One running, as before: three, the running one never cut.
	b = Assemble([]cycleModel.Cycle{n5, next, early, n4, web}, nil, &projectDomain.BacklogRead{}, now, nil)
	got = got[:0]
	for _, c := range b.Cycles {
		got = append(got, c.Number)
	}
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 3 {
		t.Fatalf("one running cycle and the next two open ones: %v", got)
	}
}

func TestRunningAcrossListsBothCyclesOfAProject(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	project := uuid.New()
	web, app, _ := sideBySide(project, now)
	saved := []any{runningCycles, cycleMembers, readProjects, standingsOf, taskStatuses}
	t.Cleanup(func() {
		runningCycles = saved[0].(func(context.Context, []uuid.UUID, time.Time) ([]cycleModel.Cycle, error))
		cycleMembers = saved[1].(func(context.Context, []uuid.UUID) (map[uuid.UUID][]string, error))
		readProjects = saved[2].(func(context.Context, string) ([]*dgraphStruct.DgraphProject, error))
		standingsOf = saved[3].(func(context.Context, string, []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error))
		taskStatuses = saved[4].(func(context.Context, []string) ([]*dgraphStruct.DgraphTask, error))
	})
	runningCycles = func(context.Context, []uuid.UUID, time.Time) ([]cycleModel.Cycle, error) {
		return []cycleModel.Cycle{app, web}, nil
	}
	readProjects = func(context.Context, string) ([]*dgraphStruct.DgraphProject, error) {
		return []*dgraphStruct.DgraphProject{{Uuid: project.String(), Name: "Launch"}}, nil
	}
	standingsOf = func(context.Context, string, []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error) {
		return map[string]workAccess.Standing{strings.ToLower(project.String()): {ProjectUUID: project.String(), Role: workAccess.RoleMember}}, nil
	}
	cycleMembers = func(context.Context, []uuid.UUID) (map[uuid.UUID][]string, error) {
		return map[uuid.UUID][]string{web.Id: {"t1"}, app.Id: {"t2"}}, nil
	}
	taskStatuses = func(context.Context, []string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{{Uuid: "t1", Status: "done"}, {Uuid: "t2", Status: "todo"}}, nil
	}
	got, err := RunningAcross(context.Background(), "reader", "0xreader", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Id != app.Id || got[1].Id != web.Id || got[0].ProjectName != "Launch" || got[1].ProjectName != "Launch" {
		t.Fatalf("both of Launch's running cycles, soonest to end first: %+v", got)
	}
	if got[0].Progress.Total != 1 || got[1].Progress.Done != 1 {
		t.Fatalf("each counts its own tasks: %+v %+v", got[0].Progress, got[1].Progress)
	}
}

func TestTheFeedWritesBothSideBySideCycles(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	web, app, _ := sideBySide(uuid.New(), now)
	out := string(CyclesICS("Launch", []View{{Cycle: web}, {Cycle: app}}, now))
	for _, want := range []string{"SUMMARY:Launch: Web", "SUMMARY:Launch: App", "UID:cycle-" + web.Id.String(), "UID:cycle-" + app.Id.String()} {
		if !strings.Contains(out, want) {
			t.Fatalf("the feed lacks %q:\n%s", want, out)
		}
	}
}
