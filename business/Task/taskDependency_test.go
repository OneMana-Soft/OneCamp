package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
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

// A moved to run from the 10th to the 14th; B waits on it in each way.
func TestPlanShiftsByKindAndLag(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		name               string
		kind               string
		lag                int
		start, due         int
		wantStart, wantDue int // 0: B stays where it is
	}{
		// The day after A's last, two days on: the 17th.
		{"finish to start, 2 days after", FinishToStart, 2, 12, 13, 17, 18},
		// No earlier than A's first day, the 10th: B already starts later.
		{"start to start", StartToStart, 0, 12, 13, 0, 0},
		{"start to start, 3 days after", StartToStart, 3, 12, 13, 13, 14},
		// B ends no earlier than A's last day, keeping its three days.
		{"finish to finish", FinishToFinish, 0, 8, 11, 11, 14},
		// B ends no earlier than A's first day.
		{"start to finish", StartToFinish, 0, 5, 7, 8, 10},
		// Two days ahead: B may start on the 13th, and does already.
		{"finish to start, 2 days ahead", FinishToStart, -2, 13, 15, 0, 0},
		// An edge with no kind is finish to start.
		{"no kind", "", 0, 12, 13, 15, 16},
	}
	for _, c := range cases {
		b := waits("B", at(loc, 2026, 10, c.start, 9), at(loc, 2026, 10, c.due, 17))
		b.BlockedBy = []*dgraphStruct.DgraphTask{{Uuid: "A", DependencyKind: c.kind, DependencyLag: c.lag}}
		tasks := []*dgraphStruct.DgraphTask{waits("A", at(loc, 2026, 10, 10, 9), at(loc, 2026, 10, 14, 17)), b}
		got, moved := shiftsOf(PlanShifts(tasks, "A", loc))["B"]
		if c.wantStart == 0 {
			if moved {
				t.Errorf("%s: B moved to %v–%v", c.name, got.Start, got.Due)
			}
			continue
		}
		if !moved || !got.Start.Equal(*at(loc, 2026, 10, c.wantStart, 9)) || !got.Due.Equal(*at(loc, 2026, 10, c.wantDue, 17)) {
			t.Errorf("%s: B at %v–%v, want the %d–%d", c.name, got.Start, got.Due, c.wantStart, c.wantDue)
		}
	}
}

// PR2 (#14), DG10: moving a task pushes its chain along in one commit (one
// Postgres statement inside it, one graph upsert) guarded by the dates it
// read, each task keeping its history line; a task the person may not move
// (here, another project's they only read) stays.
func TestShiftingAChainWritesOnce(t *testing.T) {
	loc := time.UTC
	a, b, c, other := chainOf(loc)
	stubShift(t, []*dgraphStruct.DgraphTask{a, b, c, other}, map[string]bool{other.Uuid: false})
	var commits [][]*dgraphStruct.DgraphTask
	var guards [][]domain.MoveGuard
	shiftSeams.commit = func(_ context.Context, ids []uuid.UUID, _ time.Time, ws []*dgraphStruct.DgraphTask, gs []domain.MoveGuard) error {
		if len(ids) != len(ws) || len(gs) != len(ws) {
			t.Fatalf("a row and a guard for each write: %d %d %d", len(ids), len(ws), len(gs))
		}
		commits, guards = append(commits, ws), append(guards, gs)
		return nil
	}
	afters := 0
	shiftSeams.after = func(_ context.Context, ms []movedTask, _ *dgraphStruct.DgraphUser) { afters += len(ms) }
	moved, err := moveAlong(context.Background(), a.Uuid, nil, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil || len(moved) != 2 {
		t.Fatalf("moved %+v %v", moved, err)
	}
	if len(commits) != 1 || len(commits[0]) != 2 || afters != 2 {
		t.Fatalf("one commit for both, and both told: %d commits, %d told", len(commits), afters)
	}
	for i, w := range commits[0] {
		if w.Uid != "0x2" && w.Uid != "0x3" || len(w.Activity) == 0 {
			t.Fatalf("each moved task by its uid, with its history: %+v", w)
		}
		if g := guards[0][i]; g.Uid != w.Uid || g.Start == nil || g.Start.Day() == w.StartDate.Day() {
			t.Fatalf("guarded by its dates as read: %+v against %+v", g, w)
		}
	}
}

// MoveTaskDates writes the dragged task and its chain in the same commit.
func TestAMoveAndItsChainAreOneCommit(t *testing.T) {
	loc := time.UTC
	a, b, c, other := chainOf(loc)
	stubShift(t, []*dgraphStruct.DgraphTask{a, b, c, other}, nil)
	n := 0
	var wrote []*dgraphStruct.DgraphTask
	shiftSeams.commit = func(_ context.Context, _ []uuid.UUID, _ time.Time, ws []*dgraphStruct.DgraphTask, _ []domain.MoveGuard) error {
		n++
		wrote = ws
		return nil
	}
	shiftSeams.after = func(context.Context, []movedTask, *dgraphStruct.DgraphUser) {}
	due := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	moved, err := MoveTaskDates(context.Background(), a, TaskDates{Due: &due}, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil || n != 1 || len(wrote) != 4 || len(moved) != 3 {
		t.Fatalf("a, b, c and the other project's d in one commit (%d): wrote %d, shifted %+v %v", n, len(wrote), moved, err)
	}
	if wrote[0].Uid != a.Uid || !wrote[0].DueDate.Equal(due) {
		t.Fatalf("the dragged task first, at its new date: %+v", wrote[0])
	}
}

// A move tells the rules and webhooks watching due dates (task.changed) of
// every task whose due date it moved, the dragged one included, as one
// task's new dates do (afterDates): a timeline drag that pushes a chain along
// must not move due dates without "When a task's due date changes" rules
// hearing of it. A task the move leaves where it was tells them nothing.
func TestAMoveTellsTaskChangedOfEachDueDateItMoved(t *testing.T) {
	loc := time.UTC
	// a is dragged from the 10th to the 15th; b starts the day after a ends,
	// so it moves along; c starts on the 20th, after a's new end, and stays.
	a := waits("7a5c0000-0000-4000-8000-0000000000aa", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 10, 9))
	b := waits("7a5c0000-0000-4000-8000-0000000000bb", at(loc, 2026, 10, 11, 9), at(loc, 2026, 10, 12, 9), a.Uuid)
	c := waits("7a5c0000-0000-4000-8000-0000000000cc", at(loc, 2026, 10, 20, 9), at(loc, 2026, 10, 21, 9), a.Uuid)
	a.Uid, b.Uid, c.Uid = "0x1", "0x2", "0x3"
	stubShift(t, []*dgraphStruct.DgraphTask{a, b, c}, nil)
	shiftSeams.commit = func(context.Context, []uuid.UUID, time.Time, []*dgraphStruct.DgraphTask, []domain.MoveGuard) error {
		return nil
	}
	saved := moveChanged
	t.Cleanup(func() { moveChanged = saved })
	told := map[string][2]string{}
	moveChanged = func(_ context.Context, task *dgraphStruct.DgraphTask, field, before, after string, _ *dgraphStruct.DgraphUser) {
		if field != FieldDue {
			t.Errorf("%s: told of %q, want %q", task.Uuid, field, FieldDue)
		}
		if before != after {
			told[task.Uuid] = [2]string{before, after}
		}
	}
	due := time.Date(2026, 10, 15, 9, 0, 0, 0, loc)
	if _, err := MoveTaskDates(context.Background(), a, TaskDates{Due: &due}, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got, want := told[a.Uuid], [2]string{"2026-10-10T09:00:00Z", "2026-10-15T09:00:00Z"}; got != want {
		t.Errorf("the dragged task: told %v, want %v", got, want)
	}
	if got := told[b.Uuid]; got[0] != "2026-10-12T09:00:00Z" || got[1] == "" || got[1] == got[0] {
		t.Errorf("the task pushed along: told %v, want its due date moved from the 12th", got)
	}
	if got, ok := told[c.Uuid]; ok {
		t.Errorf("the task that stayed: told %v, want nothing", got)
	}
}

// Tasks that change under a move are read and planned again; after three
// tries the move gives up, having written nothing.
func TestAStaleMoveIsPlannedAgainThenRefused(t *testing.T) {
	loc := time.UTC
	a, b, c, other := chainOf(loc)
	reads := stubShift(t, []*dgraphStruct.DgraphTask{a, b, c, other}, nil)
	tries := 0
	shiftSeams.commit = func(context.Context, []uuid.UUID, time.Time, []*dgraphStruct.DgraphTask, []domain.MoveGuard) error {
		tries++
		return domain.ErrMoveStale
	}
	told := false
	shiftSeams.after = func(context.Context, []movedTask, *dgraphStruct.DgraphUser) { told = true }
	_, err := moveAlong(context.Background(), a.Uuid, nil, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if !errors.Is(err, ErrMoveStale) || tries != moveTries || *reads != moveTries || told {
		t.Fatalf("three reads, three tries, nothing told: %v %d %d %v", err, tries, *reads, told)
	}
}

// A person who may not change the dragged task moves nothing.
func TestAMoveOfATaskThePersonCantChangeIsRefused(t *testing.T) {
	loc := time.UTC
	a, b, c, other := chainOf(loc)
	stubShift(t, []*dgraphStruct.DgraphTask{a, b, c, other}, map[string]bool{a.Uuid: false})
	shiftSeams.commit = func(context.Context, []uuid.UUID, time.Time, []*dgraphStruct.DgraphTask, []domain.MoveGuard) error {
		t.Fatal("nothing is written")
		return nil
	}
	if _, err := moveAlong(context.Background(), a.Uuid, nil, loc, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("refused: %v", err)
	}
}

// chainOf is a (moved, due on the 10th) ← b ← c in project p, and d in
// project q waiting on a.
func chainOf(loc *time.Location) (a, b, c, d *dgraphStruct.DgraphTask) {
	a = waits("7a5c0000-0000-4000-8000-00000000000a", at(loc, 2026, 10, 1, 9), at(loc, 2026, 10, 10, 9))
	b = waits("7a5c0000-0000-4000-8000-00000000000b", at(loc, 2026, 10, 5, 9), at(loc, 2026, 10, 6, 9), a.Uuid)
	c = waits("7a5c0000-0000-4000-8000-00000000000c", at(loc, 2026, 10, 7, 9), at(loc, 2026, 10, 8, 9), b.Uuid)
	d = waits("7a5c0000-0000-4000-8000-00000000000d", at(loc, 2026, 10, 5, 9), at(loc, 2026, 10, 6, 9), a.Uuid)
	for i, x := range []*dgraphStruct.DgraphTask{a, b, c} {
		x.Uid = fmt.Sprintf("0x%d", i+1)
		x.Project = &dgraphStruct.DgraphProject{Uuid: "p"}
	}
	d.Uid, d.Project = "0x9", &dgraphStruct.DgraphProject{Uuid: "q"}
	return a, b, c, d
}

// stubShift reads tasks for every move and judges each movable and visible,
// except those named in move (false: seen, not movable). It returns a count
// of the reads.
func stubShift(t *testing.T, tasks []*dgraphStruct.DgraphTask, move map[string]bool) *int {
	t.Helper()
	old := shiftSeams
	t.Cleanup(func() { shiftSeams = old })
	reads := 0
	shiftSeams.schedule = func(context.Context, string) ([]*dgraphStruct.DgraphTask, error) {
		reads++
		return tasks, nil
	}
	shiftSeams.judge = func(_ context.Context, _, _ string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		out := map[string]workAccess.Verdict{}
		member := workAccess.Standing{ProjectUUID: "p", Role: workAccess.RoleMember, EditWho: workAccess.EditMembers}
		for _, x := range ts {
			v := workAccess.VerdictOf(member, workAccess.TaskFacts{})
			if ok, named := move[x.Uuid]; named && !ok {
				v = workAccess.VerdictOf(workAccess.Standing{ProjectUUID: "p", Role: workAccess.RoleViewer}, workAccess.TaskFacts{})
			}
			out[strings.ToLower(x.Uuid)] = v
		}
		return out, nil
	}
	return &reads
}
