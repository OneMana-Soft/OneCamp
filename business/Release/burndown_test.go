package business

import (
	"testing"
	"time"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
)

// A release's burndown runs from its first task's joining (its making when it
// has none) to the end of its target day in the reader's zone, the ideal line
// reaching nothing that day; past it, on to the day it went out or today.
// With no target day the days run to today, or the day it went out, and
// there is no ideal line.
func TestAReleasesBurndownSpan(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	made := time.Date(2026, 10, 1, 10, 0, 0, 0, ist)
	target := "2026-10-20"
	first, second := time.Date(2026, 10, 2, 18, 0, 0, 0, ist), time.Date(2026, 10, 3, 9, 0, 0, 0, ist)
	members := []cycleBusiness.SpanMember{{TaskUUID: "b", JoinedAt: second}, {TaskUUID: "a", JoinedAt: first}}
	tasks := map[string]cycleBusiness.TaskProgress{"a": {Status: dgraphStruct.TASK_STATUS_TODO}, "b": {Status: dgraphStruct.TASK_STATUS_TODO}}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, ist)
	r := releaseModel.Release{CreatedAt: made, TargetDate: &target, State: releaseModel.Planned}

	s := span(r, members, ist)
	if !s.StartsAt.Equal(first) || s.EndsAt == nil || !s.EndsAt.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, ist)) || s.CompletedAt != nil || !s.RunsOver {
		t.Fatalf("span %+v, want from the first joining to the end of 20 October there", s)
	}
	b := cycleBusiness.BurndownOf(s, members, tasks, now, ist, nil)
	last := len(b.Days) - 1
	if b.Days[0] != "2026-10-02" || b.Days[last] != target || len(b.Remaining) != 9 || b.Ideal[last] != 0 || b.Ideal[last-1] == 0 {
		t.Errorf("days %s to %s (%d known), ideal %v: want to the target day, nothing left on it", b.Days[0], b.Days[last], len(b.Remaining), b.Ideal)
	}

	// Out after its target day: the days run on to the day it went out.
	out := time.Date(2026, 10, 22, 15, 0, 0, 0, ist)
	late := r
	late.State, late.ReleasedAt = releaseModel.Released, &out
	s = span(late, members, ist)
	if s.CompletedAt == nil || !s.CompletedAt.Equal(out) {
		t.Fatalf("a release out completes when it went: %+v", s)
	}
	b = cycleBusiness.BurndownOf(s, members, tasks, out.AddDate(0, 0, 5), ist, nil)
	if last = len(b.Days) - 1; b.Days[last] != "2026-10-22" || b.Ideal[last-2] != 0 || b.Ideal[last-3] == 0 {
		t.Errorf("late: days to %s, ideal %v, want to 22 October with nothing left from the 20th", b.Days[last], b.Ideal)
	}

	// No target day: to today, no ideal line; no tasks: from its making.
	none := r
	none.TargetDate = nil
	s = span(none, nil, ist)
	if !s.StartsAt.Equal(made) || s.EndsAt != nil {
		t.Fatalf("no target, no tasks: %+v", s)
	}
	b = cycleBusiness.BurndownOf(span(none, members, ist), members, tasks, now, ist, nil)
	if b.Days[len(b.Days)-1] != "2026-10-10" || b.Ideal != nil || b.IdealHours != nil {
		t.Errorf("no target: days to %s, ideal %v, want today and none", b.Days[len(b.Days)-1], b.Ideal)
	}
}
