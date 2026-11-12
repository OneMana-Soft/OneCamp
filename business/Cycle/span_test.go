package business

import (
	"reflect"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A burndown counts any span, not only a cycle's (jira-04: a release's).
// With no end its days run to today, or to the day it was completed, with no
// ideal line; one that runs over its end runs on to today, its ideal line
// reaching nothing on its end's day; past its most days the latest are kept.
func TestBurndownOfASpan(t *testing.T) {
	utc := time.UTC
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, utc)
	now := time.Date(2026, 10, 5, 15, 0, 0, 0, utc)
	done := time.Date(2026, 10, 3, 10, 0, 0, 0, utc)
	members := []SpanMember{{TaskUUID: "a", JoinedAt: start}, {TaskUUID: "b", JoinedAt: start}, {TaskUUID: "c", JoinedAt: start.AddDate(0, 0, 2)}}
	tasks := map[string]TaskProgress{"a": {Status: dgraphStruct.TASK_STATUS_DONE, Since: &done},
		"b": {Status: dgraphStruct.TASK_STATUS_TODO}, "c": {Status: dgraphStruct.TASK_STATUS_TODO}}
	days := func(from, to int) []string {
		var out []string
		for d := from; d <= to; d++ {
			out = append(out, time.Date(2026, 10, d, 0, 0, 0, 0, utc).Format(time.DateOnly))
		}
		return out
	}

	b := BurndownOf(Span{StartsAt: start}, members, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(1, 5)) || !reflect.DeepEqual(b.Scope, []int{2, 2, 3, 3, 3}) || !reflect.DeepEqual(b.Remaining, []int{2, 2, 2, 2, 2}) {
		t.Errorf("no end: days %v scope %v remaining %v, want 1 to 5 October", b.Days, b.Scope, b.Remaining)
	}
	if b.Ideal != nil {
		t.Errorf("no end, no ideal line: %v", b.Ideal)
	}

	closed := time.Date(2026, 10, 4, 12, 0, 0, 0, utc)
	left := append([]SpanMember{}, members...)
	left[0].LeftUnfinished = true
	b = BurndownOf(Span{StartsAt: start, CompletedAt: &closed}, left, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(1, 4)) || !reflect.DeepEqual(b.Remaining, []int{2, 2, 3, 3}) {
		t.Errorf("completed on the 4th, a left unfinished: days %v remaining %v", b.Days, b.Remaining)
	}

	end := time.Date(2026, 10, 4, 0, 0, 0, 0, utc) // its last day is the 3rd
	b = BurndownOf(Span{StartsAt: start, EndsAt: &end, RunsOver: true}, members, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(1, 5)) || !reflect.DeepEqual(b.Ideal, []float64{1.3, 0.7, 0, 0, 0}) {
		t.Errorf("runs over its end: days %v ideal %v, want to the 5th with nothing left from the 3rd", b.Days, b.Ideal)
	}
	b = BurndownOf(Span{StartsAt: start, EndsAt: &end}, members, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(1, 3)) || !reflect.DeepEqual(b.Ideal, []float64{1.3, 0.7, 0}) {
		t.Errorf("stops at its end: days %v ideal %v", b.Days, b.Ideal)
	}
	later := time.Date(2026, 10, 9, 0, 0, 0, 0, utc)
	b = BurndownOf(Span{StartsAt: start, EndsAt: &later, RunsOver: true}, members, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(1, 8)) || len(b.Remaining) != 5 || b.Ideal[7] != 0 || b.Ideal[6] == 0 {
		t.Errorf("an end still to come: days %v remaining %v ideal %v", b.Days, b.Remaining, b.Ideal)
	}

	b = BurndownOf(Span{StartsAt: start, MostDays: 3}, members, tasks, now, utc, nil)
	if !reflect.DeepEqual(b.Days, days(3, 5)) || !reflect.DeepEqual(b.Scope, []int{3, 3, 3}) {
		t.Errorf("past its most days: days %v scope %v, want the latest three", b.Days, b.Scope)
	}
}
