package business

import (
	"testing"
	"time"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

func TestState(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	c := cycleModel.Cycle{StartsAt: at(5), EndsAt: at(19)}
	for now, want := range map[time.Time]string{at(4): "upcoming", at(5): "current", at(18): "current", at(19): "ended"} {
		if got := State(c, now); got != want {
			t.Errorf("%v: got %s, want %s", now, got, want)
		}
	}
	done := at(20)
	c.CompletedAt = &done
	if State(c, at(10)) != "completed" {
		t.Error("completed cycle not completed")
	}
}

func TestCount(t *testing.T) {
	p := Count([]string{"todo", "inProgress", "inReview", "done", "canceled", "backlog"})
	if p != (Progress{Total: 6, Started: 2, Done: 2}) {
		t.Fatalf("got %+v", p)
	}
}

func TestWindow(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	// Midnight in India is 18:30 the day before in UTC; the cycle keeps it.
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, ist).UTC()
	s, e, err := Window(start, 2)
	if err != nil || !s.Equal(start) || !e.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, ist)) {
		t.Fatalf("got %v %v %v", s, e, err)
	}
	for _, w := range []int{0, 9} {
		if _, _, err := Window(time.Now(), w); err == nil {
			t.Errorf("%d weeks accepted", w)
		}
	}
	if _, _, err := Window(time.Time{}, 2); err == nil {
		t.Error("no start accepted")
	}
}

// PR1 (#15): a list filtered by many cycles asks for their tasks once, for
// this project's cycles only; ids that aren't uuids are dropped, and no task
// matches nothing.
func TestAFilterByCyclesReadsTheirTasksOnce(t *testing.T) {
	old := tasksInProjectCycles
	t.Cleanup(func() { tasksInProjectCycles = old })
	calls := 0
	tasksInProjectCycles = func(project uuid.UUID, cycles []uuid.UUID) ([]string, error) {
		calls++
		if len(cycles) != 3 {
			t.Fatalf("asked for %d cycles", len(cycles))
		}
		return []string{"7a5c0000-0000-4000-8000-000000000001"}, nil
	}
	got, err := FilterClause(uuid.NewString(), []string{uuid.NewString(), "nope", uuid.NewString(), uuid.NewString()})
	if err != nil || calls != 1 || got != `eq(task_uuid, ["7a5c0000-0000-4000-8000-000000000001"])` {
		t.Fatalf("%d reads: %s %v", calls, got, err)
	}
	tasksInProjectCycles = func(uuid.UUID, []uuid.UUID) ([]string, error) { return nil, nil }
	if got, _ := FilterClause(uuid.NewString(), []string{uuid.NewString()}); got != `eq(task_uuid, "00000000-0000-0000-0000-000000000000")` {
		t.Fatalf("no task: %s", got)
	}
}
