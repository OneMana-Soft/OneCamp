package business

import (
	"fmt"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// at is a time in loc; zero is an unset date.
func at(loc *time.Location, y int, m time.Month, d, h int) *time.Time {
	t := time.Date(y, m, d, h, 0, 0, 0, loc)
	return &t
}

func waits(id string, start, due *time.Time, on ...string) *dgraphStruct.DgraphTask {
	t := &dgraphStruct.DgraphTask{Uuid: id, Status: "todo", StartDate: start, DueDate: due}
	for _, b := range on {
		t.BlockedBy = append(t.BlockedBy, &dgraphStruct.DgraphTask{Uuid: b})
	}
	return t
}

func shiftsOf(plan []ShiftedTask) map[string]ShiftedTask {
	out := map[string]ShiftedTask{}
	for _, s := range plan {
		out[s.UUID] = s
	}
	return out
}

func TestPlanShiftsMovesAChainJustFarEnough(t *testing.T) {
	loc := time.UTC
	// A was moved to be due on the 10th. B (3 days, waits on A) started on
	// the 8th; C (waits on B) started on the 20th, already after B's new end.
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", at(loc, 2026, 10, 6, 9), at(loc, 2026, 10, 10, 17)),
		waits("B", at(loc, 2026, 10, 8, 9), at(loc, 2026, 10, 10, 17), "A"),
		waits("C", at(loc, 2026, 10, 20, 9), at(loc, 2026, 10, 22, 17), "B"),
	}
	got := shiftsOf(PlanShifts(tasks, "A", loc))
	b, ok := got["B"]
	if !ok || !b.Start.Equal(*at(loc, 2026, 10, 11, 9)) || !b.Due.Equal(*at(loc, 2026, 10, 13, 17)) {
		t.Fatalf("B starts the day after A is due and keeps its 3 days and its hours: %+v", b)
	}
	if _, moved := got["C"]; moved {
		t.Fatal("C already started after B's new due day and must stay")
	}
	if tasks[1].StartDate.Day() != 8 {
		t.Fatal("the schedule passed in was changed")
	}
}

func TestPlanShiftsPushesDownTheChain(t *testing.T) {
	loc := time.UTC
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(loc, 2026, 10, 15, 17)),
		waits("B", nil, at(loc, 2026, 10, 12, 17), "A"),
		waits("C", at(loc, 2026, 10, 13, 9), at(loc, 2026, 10, 14, 17), "B"),
	}
	got := shiftsOf(PlanShifts(tasks, "A", loc))
	if b := got["B"]; b.Start != nil || !b.Due.Equal(*at(loc, 2026, 10, 16, 17)) {
		t.Fatalf("a task with only a due date moves its due date: %+v", b)
	}
	if c := got["C"]; !c.Start.Equal(*at(loc, 2026, 10, 17, 9)) || !c.Due.Equal(*at(loc, 2026, 10, 18, 17)) {
		t.Fatalf("C moves after B's new day: %+v", c)
	}
}

func TestPlanShiftsWaitsForTheLastOfSeveralBlockers(t *testing.T) {
	loc := time.UTC
	// A moved to be due on the 10th; B waits on A and moves to the 11th-14th;
	// D waits on both, so it starts after B's new due day, not A's.
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(loc, 2026, 10, 10, 17)),
		waits("B", at(loc, 2026, 10, 9, 9), at(loc, 2026, 10, 12, 17), "A"),
		waits("D", at(loc, 2026, 10, 9, 9), at(loc, 2026, 10, 9, 17), "A", "B"),
	}
	got := shiftsOf(PlanShifts(tasks, "A", loc))
	if b := got["B"]; !b.Due.Equal(*at(loc, 2026, 10, 14, 17)) {
		t.Fatalf("B moves after A: %+v", b)
	}
	if d := got["D"]; !d.Start.Equal(*at(loc, 2026, 10, 15, 9)) {
		t.Fatalf("D starts after the later of its two blockers, at their new dates: %+v", d)
	}
}

func TestPlanShiftsLeavesConflictsTheMoveDidNotCause(t *testing.T) {
	loc := time.UTC
	// W waits on M and on X. X is due on the 30th, after W starts: a conflict
	// someone left. M moves from the 5th to the 6th, still before W: nothing
	// that moved reaches W, so W stays, X's conflict and all.
	tasks := []*dgraphStruct.DgraphTask{
		waits("M", nil, at(loc, 2026, 10, 6, 17)),
		waits("X", nil, at(loc, 2026, 10, 30, 17)),
		waits("W", at(loc, 2026, 10, 10, 9), at(loc, 2026, 10, 12, 17), "M", "X"),
	}
	if plan := PlanShifts(tasks, "M", loc); len(plan) != 0 {
		t.Fatalf("a task that didn't move pushed W: %+v", plan)
	}
	// When M does reach W, W moves just past M: X didn't move, so it doesn't count.
	tasks[0] = waits("M", nil, at(loc, 2026, 10, 11, 17))
	if w := shiftsOf(PlanShifts(tasks, "M", loc))["W"]; !w.Start.Equal(*at(loc, 2026, 10, 12, 9)) {
		t.Fatalf("W starts the day after M, not after X: %+v", w)
	}
}

func TestPlanShiftsFinishedWorkHoldsNothingUp(t *testing.T) {
	loc := time.UTC
	// M is done, so moving it holds nothing up; nor does B, done in the middle of a chain.
	done := func(x *dgraphStruct.DgraphTask) *dgraphStruct.DgraphTask {
		x.Status = dgraphStruct.TASK_STATUS_DONE
		return x
	}
	tasks := []*dgraphStruct.DgraphTask{
		done(waits("M", nil, at(loc, 2026, 10, 20, 17))),
		waits("W", at(loc, 2026, 10, 10, 9), at(loc, 2026, 10, 12, 17), "M"),
	}
	if plan := PlanShifts(tasks, "M", loc); len(plan) != 0 {
		t.Fatalf("a finished task pushed W: %+v", plan)
	}
	tasks = []*dgraphStruct.DgraphTask{
		waits("A", nil, at(loc, 2026, 10, 20, 17)),
		done(waits("B", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 2, 17), "A")),
		waits("C", at(loc, 2026, 10, 3, 9), at(loc, 2026, 10, 4, 17), "B"),
	}
	if plan := PlanShifts(tasks, "A", loc); len(plan) != 0 {
		t.Fatalf("finished B stays and holds C to nothing: %+v", plan)
	}
}

func TestPlanShiftsLeavesFinishedUndatedAndUnrelatedWork(t *testing.T) {
	loc := time.UTC
	done := waits("done", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 2, 17), "A")
	done.Status = dgraphStruct.TASK_STATUS_DONE
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(loc, 2026, 10, 10, 17)),
		done,
		waits("undated", nil, nil, "A"),
		waits("other", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 2, 17)),
	}
	if plan := PlanShifts(tasks, "A", loc); len(plan) != 0 {
		t.Fatalf("nothing should move: %+v", plan)
	}
}

func TestPlanShiftsKeepsHoursAcrossAClockChange(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data")
	}
	// Clocks go back on 1 Nov 2026: B moves from 31 Oct over the change.
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(ny, 2026, 11, 1, 17)),
		waits("B", at(ny, 2026, 10, 31, 9), at(ny, 2026, 10, 31, 17), "A"),
	}
	b := shiftsOf(PlanShifts(tasks, "A", ny))["B"]
	if b.Start == nil || b.Start.In(ny).Hour() != 9 || b.Start.In(ny).Day() != 2 || b.Due.In(ny).Hour() != 17 {
		t.Fatalf("B lands on 2 Nov at the same hours: %+v", b)
	}
}

func TestPlanShiftsCountsDaysInTheGivenZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no time zone data")
	}
	// A is due at 23:00 on the 10th in Kolkata (17:30 UTC, the same day);
	// B starts at 01:00 on the 11th in Kolkata (19:30 UTC on the 10th).
	// In Kolkata's days B already starts the day after: nothing moves.
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(kolkata, 2026, 10, 10, 23)),
		waits("B", at(kolkata, 2026, 10, 11, 1), at(kolkata, 2026, 10, 12, 17), "A"),
	}
	if plan := PlanShifts(tasks, "A", kolkata); len(plan) != 0 {
		t.Fatalf("B starts the next day in Kolkata: %+v", plan)
	}
	if plan := PlanShifts(tasks, "A", time.UTC); len(plan) != 1 {
		t.Fatalf("in UTC both fall on the 10th, so B moves: %+v", plan)
	}
}

func TestPlanShiftsEndsOnALoop(t *testing.T) {
	loc := time.UTC
	// The server refuses loops, but data written before that rule must not
	// hang a move, nor push the tasks in it ever later. A waits on B and B on
	// A; and further down, X waits on M and Y, and Y waits on X.
	tasks := []*dgraphStruct.DgraphTask{
		waits("A", nil, at(loc, 2026, 10, 10, 17), "B"),
		waits("B", nil, at(loc, 2026, 10, 9, 17), "A"),
		waits("M", nil, at(loc, 2026, 10, 20, 17)),
		waits("X", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 2, 17), "M", "Y"),
		waits("Y", at(loc, 2026, 10, 3, 9), at(loc, 2026, 10, 4, 17), "X"),
	}
	done := make(chan [2][]ShiftedTask, 1)
	go func() { done <- [2][]ShiftedTask{PlanShifts(tasks, "A", loc), PlanShifts(tasks, "M", loc)} }()
	select {
	case plans := <-done:
		if len(plans[0]) > 1 {
			t.Fatalf("a loop through the moved task moved more than the task waiting: %+v", plans[0])
		}
		if len(plans[1]) != 0 {
			t.Fatalf("a loop further down moved: %+v", plans[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a dependency loop never finished")
	}
}

func TestPlanShiftsResolvesALongChainInOnePass(t *testing.T) {
	loc := time.UTC
	// 100 tasks, each waiting on a kickoff and on the one before, listed last
	// first. After kickoff moves, none may start before a task it waits on is due.
	const n = 100
	tasks := []*dgraphStruct.DgraphTask{waits("k", nil, at(loc, 2026, 10, 10, 17))}
	for i := n - 1; i >= 0; i-- {
		on := []string{"k"}
		if i > 0 {
			on = append(on, fmt.Sprint(i-1))
		}
		tasks = append(tasks, waits(fmt.Sprint(i), at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 1, 17), on...))
	}
	now := map[string]*dgraphStruct.DgraphTask{}
	for _, x := range tasks {
		now[x.Uuid] = x
	}
	plan := PlanShifts(tasks, "k", loc)
	for _, s := range plan {
		moved := *now[s.UUID]
		moved.StartDate, moved.DueDate = s.Start, s.Due
		now[s.UUID] = &moved
	}
	if len(plan) != n {
		t.Fatalf("every task moves once: %d moves", len(plan))
	}
	for _, x := range now {
		for _, b := range x.BlockedBy {
			if !now[x.Uuid].StartDate.After(*now[b.Uuid].DueDate) {
				t.Fatalf("%s still starts before %s is due", x.Uuid, b.Uuid)
			}
		}
	}
}
