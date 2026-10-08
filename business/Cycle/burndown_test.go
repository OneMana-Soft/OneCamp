package business

import (
	"reflect"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

func TestBuildBurndown(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	at := func(s string) time.Time { v, _ := time.ParseInLocation("2006-01-02 15:04", s, ist); return v }
	ptr := func(v time.Time) *time.Time { return &v }
	// Two weeks from Monday 5 October; it's Thursday the 8th, midday.
	c := cycleModel.Cycle{StartsAt: at("2026-10-05 00:00"), EndsAt: at("2026-10-19 00:00")}
	members := []cycleModel.Member{
		{TaskUUID: "a", AddedAt: at("2026-10-01 09:00")}, // planned before the start
		{TaskUUID: "b", AddedAt: at("2026-10-05 10:00")},
		{TaskUUID: "c", AddedAt: at("2026-10-07 09:00")}, // added mid-cycle
		{TaskUUID: "archived", AddedAt: at("2026-10-05 10:00")},
		{TaskUUID: "e", AddedAt: at("2026-10-05 09:00")},
	}
	tasks := map[string]TaskProgress{
		"a": {Status: dgraphStruct.TASK_STATUS_DONE, Since: ptr(at("2026-10-06 15:00")), Hours: 2, HasEstimate: true},
		"b": {Status: dgraphStruct.TASK_STATUS_INPROGRESS, Hours: 3, HasEstimate: true},
		"c": {Status: dgraphStruct.TASK_STATUS_CANCELED, Since: ptr(at("2026-10-08 10:00"))},
		// Done with no record of when: done since it joined.
		"e": {Status: dgraphStruct.TASK_STATUS_DONE},
	}
	b := BuildBurndown(c, members, tasks, at("2026-10-08 12:00"), ist)

	if len(b.Days) != 14 || b.Days[0] != "2026-10-05" || b.Days[13] != "2026-10-18" {
		t.Fatalf("days %v", b.Days)
	}
	if !reflect.DeepEqual(b.Scope, []int{3, 3, 4, 4}) || !reflect.DeepEqual(b.Remaining, []int{2, 1, 2, 1}) {
		t.Errorf("scope %v remaining %v, want [3 3 4 4] [2 1 2 1]", b.Scope, b.Remaining)
	}
	// An even pace: a fourteenth of the first day's 3 done each day, none left by the end.
	if len(b.Ideal) != 14 || b.Ideal[0] != 2.8 || b.Ideal[6] != 1.5 || b.Ideal[13] != 0 {
		t.Errorf("ideal %v, want 2.8 down to 0 over 14 days", b.Ideal)
	}
	if b.Tasks != 4 || b.Done != 2 || b.Open != 1 || b.Estimated != 2 {
		t.Errorf("tasks %d done %d open %d estimated %d, want 4 2 1 2", b.Tasks, b.Done, b.Open, b.Estimated)
	}
	if !reflect.DeepEqual(b.ScopeHours, []float64{5, 5, 5, 5}) || !reflect.DeepEqual(b.RemainingHours, []float64{5, 3, 3, 3}) || b.IdealHours[0] != 4.6 {
		t.Errorf("hours: scope %v remaining %v ideal %v", b.ScopeHours, b.RemainingHours, b.IdealHours)
	}
}

func TestBurndownOfACompletedCycleEndsWithWhatItLeft(t *testing.T) {
	utc := time.UTC
	start := time.Date(2026, 9, 21, 0, 0, 0, 0, utc)
	completed := time.Date(2026, 10, 5, 10, 0, 0, 0, utc)
	c := cycleModel.Cycle{StartsAt: start, EndsAt: start.AddDate(0, 0, 14), CompletedAt: &completed}
	later := time.Date(2026, 10, 7, 0, 0, 0, 0, utc)
	members := []cycleModel.Member{
		{TaskUUID: "done", AddedAt: start},
		// Carried out unfinished, and finished later in the next cycle: still
		// remaining here to the end.
		{TaskUUID: "carried", AddedAt: start, Unfinished: true},
	}
	tasks := map[string]TaskProgress{
		"done":    {Status: dgraphStruct.TASK_STATUS_DONE, Since: &completed},
		"carried": {Status: dgraphStruct.TASK_STATUS_DONE, Since: &later},
	}
	b := BuildBurndown(c, members, tasks, time.Date(2026, 10, 8, 0, 0, 0, 0, utc), utc)
	if len(b.Remaining) != 14 || b.Remaining[13] != 2 {
		t.Fatalf("remaining %v: both tasks were still open on the last day", b.Remaining)
	}
	if b.Open != 1 || b.Done != 1 {
		t.Errorf("open %d done %d, want 1 1", b.Open, b.Done)
	}
	// Completed early: nothing after the day it was completed.
	early := start.AddDate(0, 0, 3).Add(9 * time.Hour)
	c.CompletedAt = &early
	if b := BuildBurndown(c, members, tasks, time.Date(2026, 10, 8, 0, 0, 0, 0, utc), utc); len(b.Remaining) != 4 {
		t.Errorf("completed on its fourth day: %d days of remaining work, want 4", len(b.Remaining))
	}
}

func TestBurndownBeforeTheCycleStarts(t *testing.T) {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	c := cycleModel.Cycle{StartsAt: start, EndsAt: start.AddDate(0, 0, 7)}
	members := []cycleModel.Member{{TaskUUID: "a", AddedAt: start.Add(-time.Hour)}, {TaskUUID: "b", AddedAt: start.Add(-time.Hour)}}
	tasks := map[string]TaskProgress{"a": {Status: dgraphStruct.TASK_STATUS_TODO}, "b": {Status: dgraphStruct.TASK_STATUS_TODO}}
	b := BuildBurndown(c, members, tasks, start.Add(-24*time.Hour), time.UTC)
	if len(b.Days) != 7 || len(b.Remaining) != 0 || len(b.Scope) != 0 {
		t.Fatalf("days %d remaining %v scope %v", len(b.Days), b.Remaining, b.Scope)
	}
	if b.Ideal[0] != 1.7 || b.ScopeHours != nil || b.IdealHours != nil {
		t.Errorf("ideal %v from everything in it now; no hours without estimates (%v)", b.Ideal, b.ScopeHours)
	}
}

// A cycle's end is its start plus whole days of 24 hours, so a clock change
// leaves it an hour into the next day; the cycle still has its 14 days.
func TestBurndownDaysAcrossClockChanges(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	for _, start := range []time.Time{
		time.Date(2026, 3, 2, 0, 0, 0, 0, ny),   // clocks go forward on 8 March
		time.Date(2026, 10, 26, 0, 0, 0, 0, ny), // and back on 1 November
	} {
		s, e, err := Window(start.UTC(), 2)
		if err != nil {
			t.Fatal(err)
		}
		b := BuildBurndown(cycleModel.Cycle{StartsAt: s, EndsAt: e}, nil, nil, s, ny)
		want := start.AddDate(0, 0, 13).Format(time.DateOnly)
		if len(b.Days) != 14 || b.Days[0] != start.Format(time.DateOnly) || b.Days[13] != want {
			t.Errorf("cycle from %s: %d days, %s to %s, want 14 to %s", start.Format(time.DateOnly), len(b.Days), b.Days[0], b.Days[len(b.Days)-1], want)
		}
	}
}

func TestBuildVelocity(t *testing.T) {
	done := func(n int) *time.Time { v := time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC); return &v }
	two := 2
	var cycles []cycleModel.Cycle
	members := map[uuid.UUID][]string{}
	tasks := map[string]TaskProgress{}
	// Eight completed cycles, numbered 1 to 8, and an open one: velocity looks
	// at the latest six, and averages the latest three.
	for n := 1; n <= 9; n++ {
		c := cycleModel.Cycle{Id: uuid.New(), Number: n, CarriedCount: &two}
		if n <= 8 {
			c.CompletedAt = done(n)
		}
		cycles = append(cycles, c)
		for i := 0; i < n; i++ {
			id := c.Id.String() + string(rune('a'+i))
			members[c.Id] = append(members[c.Id], id)
			tasks[id] = TaskProgress{Status: dgraphStruct.TASK_STATUS_DONE, Hours: 1, HasEstimate: true}
		}
		// A cancelled task isn't velocity.
		cancelled := c.Id.String() + "-x"
		members[c.Id] = append(members[c.Id], cancelled)
		tasks[cancelled] = TaskProgress{Status: dgraphStruct.TASK_STATUS_CANCELED}
	}
	// Given out of order, as a list might be.
	cycles[0], cycles[7] = cycles[7], cycles[0]
	v := BuildVelocity(cycles, members, tasks)
	var numbers, doneCounts []int
	for _, d := range v.Cycles {
		numbers = append(numbers, d.Number)
		doneCounts = append(doneCounts, d.Done)
	}
	if !reflect.DeepEqual(numbers, []int{3, 4, 5, 6, 7, 8}) || !reflect.DeepEqual(doneCounts, []int{3, 4, 5, 6, 7, 8}) {
		t.Fatalf("cycles %v done %v", numbers, doneCounts)
	}
	if v.Typical == nil || *v.Typical != 7 || v.TypicalHours == nil || *v.TypicalHours != 7 || v.Cycles[0].Unfinished != 2 {
		t.Errorf("typical %v hours %v unfinished %d", v.Typical, v.TypicalHours, v.Cycles[0].Unfinished)
	}
	if v := BuildVelocity(nil, nil, nil); v.Typical != nil || v.Cycles == nil {
		t.Errorf("no completed cycles: %+v", v)
	}
}

// A reader zones away from whoever made the cycle sees every day it touches
// in their own zone, the last part-day included.
func TestBurndownForAReaderInAnotherZone(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	london, _ := time.LoadLocation("Europe/London")
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, ist) // Sunday 4 October, 19:30 in London
	c := cycleModel.Cycle{StartsAt: start, EndsAt: start.AddDate(0, 0, 14)}
	done := time.Date(2026, 10, 18, 10, 0, 0, 0, london) // still inside the cycle there
	members := []cycleModel.Member{{TaskUUID: "a", AddedAt: start.Add(-time.Hour)}, {TaskUUID: "b", AddedAt: start.Add(-time.Hour)}}
	tasks := map[string]TaskProgress{"a": {Status: dgraphStruct.TASK_STATUS_DONE, Since: &done}, "b": {Status: dgraphStruct.TASK_STATUS_TODO}}
	b := BuildBurndown(c, members, tasks, time.Date(2026, 10, 18, 12, 0, 0, 0, london), london)
	if len(b.Days) != 15 || b.Days[0] != "2026-10-04" || b.Days[14] != "2026-10-18" {
		t.Fatalf("%d days, %s to %s, want 15 from 4 to 18 October", len(b.Days), b.Days[0], b.Days[len(b.Days)-1])
	}
	if len(b.Remaining) != 15 || b.Remaining[14] != 1 || b.Open != 1 {
		t.Errorf("the task done on the last morning: remaining %v, open %d", b.Remaining, b.Open)
	}
	// The demo's cycles start at midnight UTC: in New York, a Sunday evening.
	utc := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	ny, _ := time.LoadLocation("America/New_York")
	b = BuildBurndown(cycleModel.Cycle{StartsAt: utc, EndsAt: utc.AddDate(0, 0, 14)}, nil, nil, utc, ny)
	if len(b.Days) != 15 || b.Days[0] != "2026-10-04" || b.Days[14] != "2026-10-18" {
		t.Errorf("New York: %d days, %s to %s", len(b.Days), b.Days[0], b.Days[len(b.Days)-1])
	}
}

// Chile's clocks go forward at midnight on 6 September 2026: that day has no
// midnight, and the days must still be named once each.
func TestBurndownDaysWhereClocksChangeAtMidnight(t *testing.T) {
	santiago, _ := time.LoadLocation("America/Santiago")
	start := time.Date(2026, 8, 31, 0, 0, 0, 0, santiago)
	b := BuildBurndown(cycleModel.Cycle{StartsAt: start, EndsAt: start.AddDate(0, 0, 14)}, nil, nil, start, santiago)
	seen := map[string]bool{}
	for _, d := range b.Days {
		if seen[d] {
			t.Fatalf("%s twice in %v", d, b.Days)
		}
		seen[d] = true
	}
	if len(b.Days) != 14 || b.Days[0] != "2026-08-31" || b.Days[13] != "2026-09-13" || !seen["2026-09-06"] {
		t.Errorf("days %v", b.Days)
	}
}

// Hours are averaged only from the cycles the average covers.
func TestVelocityHoursOnlyFromTheCyclesAveraged(t *testing.T) {
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cycles []cycleModel.Cycle
	members := map[uuid.UUID][]string{}
	tasks := map[string]TaskProgress{}
	for n := 1; n <= 5; n++ {
		c := cycleModel.Cycle{Id: uuid.New(), Number: n, CompletedAt: &when}
		cycles = append(cycles, c)
		id := c.Id.String()
		members[c.Id] = []string{id}
		// Only the first two cycles' work had estimates.
		tasks[id] = TaskProgress{Status: dgraphStruct.TASK_STATUS_DONE, Hours: 3, HasEstimate: n <= 2}
		if n > 2 {
			tasks[id] = TaskProgress{Status: dgraphStruct.TASK_STATUS_DONE}
		}
	}
	if v := BuildVelocity(cycles, members, tasks); v.TypicalHours != nil || v.Typical == nil || *v.Typical != 1 {
		t.Errorf("typical %v, hours %v: no hours from cycles outside the average", v.Typical, v.TypicalHours)
	}
}
