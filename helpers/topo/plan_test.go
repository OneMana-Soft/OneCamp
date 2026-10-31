package topo

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/timing"
)

// d is a civil day in October 2026.
func d(day int) Day { return CivilDay(time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC), time.UTC) }

func task(id string, start, end int) Task {
	return Task{ID: id, Start: d(start), End: d(end), Dated: true}
}

func link(from, to string, kind Kind, lag int) Link {
	return Link{From: from, To: to, Kind: kind, Lag: lag}
}

func TestCivilDayIsTheCalendarDayInTheZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// 02:30 UTC on 1 Nov is still 31 Oct in New York.
	at := time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC)
	if got, want := CivilDay(at, ny), CivilDay(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), time.UTC); got != want {
		t.Fatalf("31 Oct in New York: %v, want %v", got, want)
	}
	if CivilDay(at, ny).String() != "2026-10-31" {
		t.Fatalf("written as a date: %s", CivilDay(at, ny))
	}
}

// A day count across a clock change is still whole days: New York's 1 Nov
// 2026 has 25 hours, London's 25 Oct too; the days between stay exact.
func TestDaysAcrossAClockChangeAreWhole(t *testing.T) {
	for _, name := range []string{"America/New_York", "Europe/London", "Australia/Sydney", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skip(err)
		}
		before := time.Date(2026, 10, 24, 23, 30, 0, 0, loc)
		after := time.Date(2026, 11, 2, 0, 30, 0, 0, loc)
		if n := int(CivilDay(after, loc) - CivilDay(before, loc)); n != 9 {
			t.Fatalf("%s: 24 Oct to 2 Nov is 9 days, got %d", name, n)
		}
		// A schedule across the change: the chain's slack is in whole days.
		s := Schedule{Tasks: []Task{
			{ID: "a", Start: CivilDay(before, loc), End: CivilDay(before, loc), Dated: true},
			{ID: "b", Start: CivilDay(after, loc), End: CivilDay(after, loc), Dated: true},
		}, Links: []Link{link("a", "b", FinishToStart, 0)}}
		p, err := CriticalPath(s)
		if err != nil {
			t.Fatal(err)
		}
		if p.Slots["a"].Slack != 8 || p.Slots["b"].Slack != 0 {
			t.Fatalf("%s: a can slip 8 days before b must move: %+v", name, p.Slots)
		}
	}
}

func TestCriticalPathOfAChain(t *testing.T) {
	// a (1-3) → b (4-6) → d (10-12); c (4-5) → d, with room to spare.
	s := Schedule{
		Tasks: []Task{task("a", 1, 3), task("b", 4, 6), task("c", 4, 5), task("d", 10, 12)},
		Links: []Link{link("a", "b", FinishToStart, 0), link("b", "d", FinishToStart, 2), link("a", "c", FinishToStart, 0), link("c", "d", FinishToStart, 0)},
	}
	p, err := CriticalPath(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Finish != d(12) {
		t.Fatalf("finishes with d on the 12th: %v", p.Finish)
	}
	// b's latest end: d starts on the 10th, less a 2-day lag, less the day
	// after: the 7th. So b has 1 day; c has 4; a has the least of its paths.
	want := map[string]int{"a": 1, "b": 1, "c": 4, "d": 0}
	for id, slack := range want {
		if got := p.Slots[id].Slack; got != slack {
			t.Fatalf("%s's slack: %d, want %d (%+v)", id, got, slack, p.Slots[id])
		}
	}
	if !p.Slots["d"].Critical || p.Slots["a"].Critical {
		t.Fatalf("only d is critical: %+v", p.Slots)
	}
	if p.Slots["c"].FreeSlack != 4 || p.Slots["a"].FreeSlack != 0 {
		t.Fatalf("c can slip 4 days before d must; a none before b must: %+v", p.Slots)
	}
	if p.Slots["b"].LatestEnd != d(7) {
		t.Fatalf("b's latest end is the 7th: %v", p.Slots["b"].LatestEnd)
	}
}

func TestEveryKindAndLagBindsTheBackwardPass(t *testing.T) {
	// Each case: blocker x (2-4, 3 days), waiting y (10-12), finish on the
	// 12th with y critical. x's latest end is what the kind and lag allow.
	for _, c := range []struct {
		kind Kind
		lag  int
		want int // x's latest end, as a day of October
	}{
		{FinishToStart, 0, 9},   // y starts the day after x ends
		{FinishToStart, 2, 7},   // two days' lag
		{FinishToStart, -1, 10}, // a day ahead
		{StartToStart, 0, 12},   // x may start as late as y's start: 10, ends 12
		{StartToStart, 3, 9},    // starts by the 7th, ends the 9th
		{FinishToFinish, 0, 12}, // x ends by y's end
		{FinishToFinish, 1, 11},
		{StartToFinish, 0, 14}, // x starts by y's end (12), so ends by 14, capped by the finish
		{StartToFinish, 4, 10}, // starts by the 8th, ends the 10th
	} {
		s := Schedule{Tasks: []Task{task("x", 2, 4), task("y", 10, 12)}, Links: []Link{link("x", "y", c.kind, c.lag)}}
		p, err := CriticalPath(s)
		if err != nil {
			t.Fatal(err)
		}
		want := d(c.want)
		if want > p.Finish {
			want = p.Finish
		}
		if got := p.Slots["x"].LatestEnd; got != want {
			t.Fatalf("%s lag %d: x's latest end %v, want %v", c.kind, c.lag, got, want)
		}
		if p.Slots["x"].Slack != int(want-d(4)) {
			t.Fatalf("%s lag %d: slack %d", c.kind, c.lag, p.Slots["x"].Slack)
		}
	}
}

func TestEveryKindAndLagBindsTheForwardPass(t *testing.T) {
	// y (5-6) waits on x (2-4): is y late, by the kind?
	for _, c := range []struct {
		kind     Kind
		lag      int
		earliest int
		late     bool
	}{
		{FinishToStart, 0, 5, false},
		{FinishToStart, 1, 6, true},
		{StartToStart, 0, 2, false},
		{StartToStart, 4, 6, true},
		{FinishToFinish, 0, 3, false}, // ends by 4 at the earliest: 2 days, so starts on the 3rd
		{FinishToFinish, 3, 6, true},
		{StartToFinish, 0, 1, false},
		{StartToFinish, 6, 7, true},
	} {
		s := Schedule{Tasks: []Task{task("x", 2, 4), task("y", 5, 6)}, Links: []Link{link("x", "y", c.kind, c.lag)}}
		p, err := CriticalPath(s)
		if err != nil {
			t.Fatal(err)
		}
		y := p.Slots["y"]
		if y.EarliestStart == nil || *y.EarliestStart != d(c.earliest) || y.Late != c.late {
			t.Fatalf("%s lag %d: earliest %v late %v, want %v %v", c.kind, c.lag, y.EarliestStart, y.Late, d(c.earliest), c.late)
		}
	}
}

func TestABrokenDependencyIsBehindAndCritical(t *testing.T) {
	// b starts before a ends: the plan is already behind by 2 days.
	s := Schedule{Tasks: []Task{task("a", 1, 5), task("b", 4, 8)}, Links: []Link{link("a", "b", FinishToStart, 0)}}
	p, err := CriticalPath(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Slots["a"].Slack != -2 || !p.Slots["a"].Critical || !p.Slots["b"].Late {
		t.Fatalf("a is 2 days behind, b late: %+v", p.Slots)
	}
}

func TestLoopsAreRefused(t *testing.T) {
	s := Schedule{
		Tasks: []Task{task("a", 1, 2), task("b", 3, 4), task("c", 5, 6), task("free", 1, 9)},
		Links: []Link{link("a", "b", FinishToStart, 0), link("b", "c", FinishToStart, 0), link("c", "a", FinishToStart, 0)},
	}
	p, err := CriticalPath(s)
	var loop *LoopError
	if !errors.Is(err, ErrLoop) || !errors.As(err, &loop) {
		t.Fatalf("a loop is refused: %v", err)
	}
	if !reflect.DeepEqual(loop.IDs, []string{"a", "b", "c"}) || !reflect.DeepEqual(p.Looped, []string{"a", "b", "c"}) {
		t.Fatalf("the loop's tasks are named, in the order given: %v %v", loop.IDs, p.Looped)
	}
	if _, ok := p.Slots["a"]; ok {
		t.Fatal("a looped task gets no slack")
	}
	if p.Slots["free"].Slack != 0 || !p.Slots["free"].Critical {
		t.Fatalf("the rest is still planned: %+v", p.Slots["free"])
	}
	if len(Shift(s, "a", 500)) != 0 {
		t.Fatal("a shift never comes round a loop")
	}
}

func TestTiesBreakTheSameWayEveryTime(t *testing.T) {
	// Two chains of the same length, both critical: the path is the one
	// whose first task starts first, then by id.
	s := Schedule{
		Tasks: []Task{task("q2", 1, 3), task("q1", 4, 6), task("p2", 1, 3), task("p1", 4, 6)},
		Links: []Link{link("q2", "q1", FinishToStart, 0), link("p2", "p1", FinishToStart, 0)},
	}
	var first []string
	for i := 0; i < 20; i++ {
		rand.Shuffle(len(s.Tasks), func(a, b int) { s.Tasks[a], s.Tasks[b] = s.Tasks[b], s.Tasks[a] })
		rand.Shuffle(len(s.Links), func(a, b int) { s.Links[a], s.Links[b] = s.Links[b], s.Links[a] })
		p, err := CriticalPath(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"p1", "p2", "q1", "q2"} {
			if !p.Slots[id].Critical {
				t.Fatalf("both chains are critical: %+v", p.Slots)
			}
		}
		if !p.CriticalLink("p2", "p1") || !p.CriticalLink("q2", "q1") {
			t.Fatal("both links are critical")
		}
		if first == nil {
			first = p.Path
		}
		if !reflect.DeepEqual(p.Path, []string{"p2", "p1"}) || !reflect.DeepEqual(p.Path, first) {
			t.Fatalf("the path is p2 → p1 every time: %v", p.Path)
		}
	}
}

func TestClosedAndUndatedWorkTakesNoPart(t *testing.T) {
	s := Schedule{
		Tasks: []Task{task("a", 1, 3), {ID: "done", Start: d(4), End: d(20), Dated: true, Closed: true}, {ID: "someday"}, task("b", 4, 5)},
		Links: []Link{link("a", "done", FinishToStart, 0), link("a", "someday", FinishToStart, 0), link("a", "b", FinishToStart, 0)},
	}
	p, err := CriticalPath(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Slots["done"]; ok || p.Finish != d(5) {
		t.Fatalf("a finished task doesn't set the finish: %v %+v", p.Finish, p.Slots)
	}
	if _, ok := p.Slots["someday"]; ok {
		t.Fatal("an undated task has no slot")
	}
}

// Shift is PlanShifts' arithmetic, store-agnostic: only what the move broke
// moves, just far enough, in dependency order.
func TestShiftMovesJustFarEnoughInOrder(t *testing.T) {
	// a moved to end on the 10th; b (8-10) waits fs; c (12-13) waits on b;
	// e waits ss +1 on a.
	s := Schedule{
		Tasks: []Task{task("a", 6, 10), task("b", 8, 10), task("c", 12, 13), task("e", 5, 5), task("z", 1, 2)},
		Links: []Link{link("a", "b", FinishToStart, 0), link("b", "c", FinishToStart, 0), link("a", "e", StartToStart, 1), link("z", "b", FinishToStart, 0)},
	}
	got := Shift(s, "a", 500)
	want := []Move{{ID: "e", Days: 2}, {ID: "b", Days: 3}, {ID: "c", Days: 2}}
	if !sameMoves(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := Shift(s, "a", 1); len(got) != 1 {
		t.Fatalf("a cap stops the plan: %+v", got)
	}
}

func sameMoves(a, b []Move) bool {
	m := map[string]int{}
	for _, x := range a {
		m[x.ID] = x.Days
	}
	if len(a) != len(b) {
		return false
	}
	for _, x := range b {
		if m[x.ID] != x.Days {
			return false
		}
	}
	return true
}

// The planner's budget: a 2,000-task plan with about 4,000 links of every kind
// is planned in well under the time a timeline read takes. Held as the least
// CPU time of five runs (tests/timing), since one run's wall time is mostly
// the machine's load: a single wall-clock run took 60 ms during make verify
// at load, against 4.9 ms alone (10.7 ms under the race detector), 19 Oct
// 2026. The budget is 50 ms.
func TestTwoThousandTasksWithinBudget(t *testing.T) {
	s := bigSchedule(2000, 2)
	var p Plan
	var err error
	took := timing.FastestCPU(5, func() { p, err = CriticalPath(s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Slots) != 2000 || len(p.Path) == 0 {
		t.Fatalf("every task planned, and a path: %d %d", len(p.Slots), len(p.Path))
	}
	if took > 50*time.Millisecond {
		t.Fatalf("2,000 tasks took %v of CPU, over 50 ms", took)
	}
	if took := timing.FastestCPU(5, func() { Shift(s, "t0", 500) }); took > 50*time.Millisecond {
		t.Fatalf("a shift over 2,000 tasks took %v of CPU", took)
	}
}

func bigSchedule(n, perTask int) Schedule {
	r := rand.New(rand.NewPCG(7, 9))
	kinds := []Kind{FinishToStart, StartToStart, FinishToFinish, StartToFinish}
	var s Schedule
	for i := 0; i < n; i++ {
		start := i/4 + r.IntN(5)
		s.Tasks = append(s.Tasks, Task{ID: fmt.Sprintf("t%d", i), Start: d(1) + Day(start), End: d(1) + Day(start+r.IntN(6)), Dated: true})
		for j := 0; j < perTask && i > 0; j++ {
			// Only to earlier tasks: no loops.
			s.Links = append(s.Links, Link{From: fmt.Sprintf("t%d", r.IntN(i)), To: fmt.Sprintf("t%d", i), Kind: kinds[r.IntN(4)], Lag: r.IntN(5) - 1})
		}
	}
	return s
}

func BenchmarkCriticalPath2000(b *testing.B) {
	s := bigSchedule(2000, 2)
	for i := 0; i < b.N; i++ {
		if _, err := CriticalPath(s); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHoldsIsTheConstraintTable(t *testing.T) {
	x, y := task("x", 2, 4), task("y", 5, 6)
	for _, c := range []struct {
		kind Kind
		lag  int
		want bool
	}{{FinishToStart, 0, true}, {FinishToStart, 1, false}, {StartToStart, 3, true}, {StartToStart, 4, false}, {FinishToFinish, 2, true}, {FinishToFinish, 3, false}, {StartToFinish, 4, true}, {StartToFinish, 5, false}} {
		if got := Holds(link("x", "y", c.kind, c.lag), x, y); got != c.want {
			t.Errorf("%s lag %d: %v", c.kind, c.lag, got)
		}
	}
}

// A day starts at midnight where it is read, and CivilDay takes that instant
// back to the day, across a change of the clocks too.
func TestADayStartsAtMidnightInItsZone(t *testing.T) {
	york, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	for _, loc := range []*time.Location{time.UTC, york, time.FixedZone("IST", 5*3600+1800), nil} {
		for _, at := range []time.Time{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)} {
			d := CivilDay(at, loc)
			s := d.Start(loc)
			if CivilDay(s, loc) != d || CivilDay(s.Add(-time.Nanosecond), loc) != d-1 {
				t.Errorf("%v in %v starts at %v", d, loc, s)
			}
		}
	}
	if got := Day(20755).Start(nil); !got.Equal(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("29 Oct 2026 starts at %v in UTC", got)
	}
}
