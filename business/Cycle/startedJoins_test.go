package business

import (
	"context"
	"testing"
	"time"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// joinsWorld stands in for the store: a project's schedule, its cycles, and which cycle each task is in.
type joinsWorld struct {
	schedule *cycleModel.Schedule
	cycles   []cycleModel.Cycle
	in       map[uuid.UUID]*cycleModel.Cycle
	put      map[uuid.UUID]uuid.UUID
}

func (w *joinsWorld) install(t *testing.T) {
	t.Helper()
	was := []any{scheduleOfProject, cyclesOfProject, cycleOfTask, putTaskInCycle}
	scheduleOfProject = func(context.Context, uuid.UUID) (*cycleModel.Schedule, error) { return w.schedule, nil }
	cyclesOfProject = func(uuid.UUID) ([]cycleModel.Cycle, error) { return w.cycles, nil }
	cycleOfTask = func(task uuid.UUID) (*cycleModel.Cycle, error) { return w.in[task], nil }
	putTaskInCycle = func(task uuid.UUID, cycle *uuid.UUID) error {
		w.put[task] = *cycle
		return nil
	}
	t.Cleanup(func() {
		scheduleOfProject = was[0].(func(context.Context, uuid.UUID) (*cycleModel.Schedule, error))
		cyclesOfProject = was[1].(func(uuid.UUID) ([]cycleModel.Cycle, error))
		cycleOfTask = was[2].(func(uuid.UUID) (*cycleModel.Cycle, error))
		putTaskInCycle = was[3].(func(uuid.UUID, *uuid.UUID) error)
	})
}

func TestAStartedTaskJoinsTheRunningCycle(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	project, task := uuid.New(), uuid.New()
	ended := cycleModel.Cycle{Id: uuid.New(), StartsAt: now.AddDate(0, 0, -28), EndsAt: now.AddDate(0, 0, -14)}
	running := cycleModel.Cycle{Id: uuid.New(), StartsAt: now.AddDate(0, 0, -3), EndsAt: now.AddDate(0, 0, 11)}
	next := cycleModel.Cycle{Id: uuid.New(), StartsAt: now.AddDate(0, 0, 11), EndsAt: now.AddDate(0, 0, 25)}
	on := &cycleModel.Schedule{ProjectUUID: project, AddStarted: true}

	cases := []struct {
		name     string
		schedule *cycleModel.Schedule
		cycles   []cycleModel.Cycle
		in       *cycleModel.Cycle
		want     *uuid.UUID
	}{
		{"joins the cycle running now", on, []cycleModel.Cycle{ended, running, next}, nil, &running.Id},
		{"cycles by hand: stays out", nil, []cycleModel.Cycle{running}, nil, nil},
		{"the schedule doesn't add started tasks: stays out", &cycleModel.Schedule{ProjectUUID: project}, []cycleModel.Cycle{running}, nil, nil},
		{"no cycle running now: stays out", on, []cycleModel.Cycle{ended, next}, nil, nil},
		{"planned into a later cycle: stays there", on, []cycleModel.Cycle{running, next}, &next, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &joinsWorld{schedule: c.schedule, cycles: c.cycles, in: map[uuid.UUID]*cycleModel.Cycle{task: c.in}, put: map[uuid.UUID]uuid.UUID{}}
			w.install(t)
			if err := joinRunningCycle(context.Background(), project, task, now); err != nil {
				t.Fatal(err)
			}
			got, put := w.put[task]
			switch {
			case c.want == nil && put:
				t.Fatalf("put in cycle %s, want left alone", got)
			case c.want != nil && (!put || got != *c.want):
				t.Fatalf("put in %v (%v), want %s", got, put, *c.want)
			}
		})
	}
}

func TestOnlyAMoveThatStartsATaskCounts(t *testing.T) {
	cases := []struct {
		old, next string
		want      bool
	}{
		{"todo", "inProgress", true},
		{"backlog", "inProgress", true},
		{"todo", "inReview", true},
		{"inProgress", "inReview", false},
		{"done", "inProgress", false},
		{"todo", "done", false},
		{"inProgress", "todo", false},
	}
	for _, c := range cases {
		if got := starts(c.old, c.next); got != c.want {
			t.Errorf("starts(%q, %q) = %v, want %v", c.old, c.next, got, c.want)
		}
	}
}

func TestTheListenerReadsOnlyTheServersOwnEvent(t *testing.T) {
	project, task := uuid.New(), uuid.New()
	now := time.Now()
	running := cycleModel.Cycle{Id: uuid.New(), StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}
	w := &joinsWorld{schedule: &cycleModel.Schedule{AddStarted: true}, cycles: []cycleModel.Cycle{running}, in: map[uuid.UUID]*cycleModel.Cycle{}, put: map[uuid.UUID]uuid.UUID{}}
	w.install(t)
	event := func(kind string, data map[string]interface{}) { startedJoinsCycle(context.Background(), kind, data) }
	event("task.assigned", map[string]interface{}{"task_id": task.String(), "project_id": project.String(), "old_status": "todo", "new_status": "inProgress"})
	event("task.status_changed", map[string]interface{}{"task_id": "not-a-uuid", "project_id": project.String(), "old_status": "todo", "new_status": "inProgress"})
	if len(w.put) != 0 {
		t.Fatalf("put %v from an event that isn't a start of a known task", w.put)
	}
	event("task.status_changed", map[string]interface{}{"task_id": task.String(), "project_id": project.String(), "old_status": "todo", "new_status": "inProgress"})
	if w.put[task] != running.Id {
		t.Fatalf("a started task went to %v, want %s", w.put[task], running.Id)
	}
}
