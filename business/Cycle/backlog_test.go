package business

import (
	"errors"
	"testing"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

func est(m int) *int { return &m }

func at(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

func task(id, status string, minutes int, created int) *dgraphStruct.DgraphTask {
	c := at(created)
	return &dgraphStruct.DgraphTask{Uuid: id, Status: status, EstimateMinutes: est(minutes), CreatedAt: &c}
}

// B3: the backlog's containers are the open cycles in order, at most three,
// each with its tasks (open first), their estimate and progress; capacity is
// what the latest completed cycles finished; the backlog's count and estimate
// are the graph's, over all of it, never the page's.
func TestTheBacklogIsTheOpenCyclesThenWhatIsInNone(t *testing.T) {
	done := at(1)
	c1 := cycleModel.Cycle{Id: uuid.New(), Number: 1, StartsAt: at(1).AddDate(0, 0, -28), EndsAt: at(1).AddDate(0, 0, -14), CompletedAt: &done}
	c2 := cycleModel.Cycle{Id: uuid.New(), Number: 2, StartsAt: at(1).AddDate(0, 0, -14), EndsAt: at(1), CompletedAt: &done}
	cur := cycleModel.Cycle{Id: uuid.New(), Number: 3, StartsAt: at(5), EndsAt: at(19)}
	next := cycleModel.Cycle{Id: uuid.New(), Number: 4, StartsAt: at(19), EndsAt: at(26)}
	n5 := cycleModel.Cycle{Id: uuid.New(), Number: 5, StartsAt: at(26), EndsAt: at(30)}
	n6 := cycleModel.Cycle{Id: uuid.New(), Number: 6, StartsAt: at(30), EndsAt: at(31)}
	members := map[uuid.UUID][]string{
		c1.Id:   {"old-1", "old-2"},
		c2.Id:   {"old-3"},
		cur.Id:  {"done-a", "open-b", "open-c", "gone"},
		next.Id: {"open-d"},
	}
	read := &projectDomain.BacklogRead{
		Backlog: []*dgraphStruct.DgraphTask{task("b1", "todo", 60, 2)},
		Count:   340, Minutes: 9000,
		Planned: []*dgraphStruct.DgraphTask{
			task("old-1", "done", 120, 1), task("old-2", "done", 60, 1), task("old-3", "done", 360, 1),
			task("done-a", "done", 30, 3), task("open-c", "todo", 90, 4), task("open-b", "inProgress", 60, 3),
			task("open-d", "todo", 0, 3),
		},
	}
	b := Assemble([]cycleModel.Cycle{n6, next, c1, cur, c2, n5}, members, read, at(10))
	if len(b.Cycles) != 3 || b.Cycles[0].Number != 3 || b.Cycles[1].Number != 4 || b.Cycles[2].Number != 5 {
		t.Fatalf("the open cycles in order, three at most: %+v", b.Cycles)
	}
	cy := b.Cycles[0]
	if cy.State != "current" || cy.EstimateMinutes != 180 || cy.Progress != (Progress{Total: 3, Started: 1, Done: 1}) {
		t.Fatalf("the current cycle's totals (a task the reader can't see, or deleted, counts nowhere): %+v", cy)
	}
	if ids := [3]string{cy.Tasks[0].Uuid, cy.Tasks[1].Uuid, cy.Tasks[2].Uuid}; ids != [3]string{"open-b", "open-c", "done-a"} {
		t.Fatalf("open work first, oldest first, then done: %v", ids)
	}
	if b.Count != 340 || b.EstimateMinutes != 9000 || len(b.Tasks) != 1 {
		t.Fatalf("the backlog's totals are all of it: %+v", b)
	}
	// Two completed cycles finished 3 h and 6 h: 4.5 h on average.
	if b.CapacityMinutes == nil || *b.CapacityMinutes != 270 {
		t.Fatalf("capacity: %v", b.CapacityMinutes)
	}
	if len(b.Cycles[2].Tasks) != 0 || b.Cycles[2].Tasks == nil {
		t.Fatal("an empty cycle has an empty list, never null")
	}

	// No completed cycle with estimates: no capacity.
	if b := Assemble([]cycleModel.Cycle{cur}, members, &projectDomain.BacklogRead{}, at(10)); b.CapacityMinutes != nil || b.Tasks == nil {
		t.Fatalf("no capacity before a cycle finishes estimated work: %+v", b)
	}
}

func TestTheReadAsksForTheOpenCyclesAndTheLatestCompleted(t *testing.T) {
	done := at(1)
	old := cycleModel.Cycle{Id: uuid.New(), Number: 1, CompletedAt: &done}
	open := cycleModel.Cycle{Id: uuid.New(), Number: 2}
	inOpen, all := plannedIDs([]cycleModel.Cycle{old, open}, map[uuid.UUID][]string{old.Id: {"a"}, open.Id: {"b"}})
	if len(inOpen) != 1 || inOpen[0] != "b" || len(all) != 2 {
		t.Fatalf("left out of the backlog: %v; read: %v", inOpen, all)
	}
}

// Start cycle: an upcoming cycle starts now and keeps its length; one that
// has started, or is complete, doesn't.
func TestStartingTheNextCycleKeepsItsLength(t *testing.T) {
	next := cycleModel.Cycle{StartsAt: at(19), EndsAt: at(26)}
	s, e, err := StartWindow(next, at(10))
	if err != nil || !s.Equal(at(10)) || !e.Equal(at(17)) {
		t.Fatalf("got %v %v %v", s, e, err)
	}
	var ce *CycleError
	if _, _, err := StartWindow(next, at(20)); !errors.As(err, &ce) || ce.Error() != "That cycle has already started." {
		t.Fatalf("a started cycle: %v", err)
	}
	done := at(20)
	next.CompletedAt = &done
	if _, _, err := StartWindow(next, at(10)); !errors.As(err, &ce) || ce.Error() != "That cycle is already complete." {
		t.Fatalf("a completed cycle: %v", err)
	}
}
