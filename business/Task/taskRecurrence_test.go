package business

import (
	"errors"
	"testing"
	"time"

	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
)

func TestNextOccurrence(t *testing.T) {
	at := func(m time.Month, day, h int) time.Time { return time.Date(2026, m, day, h, 0, 0, 0, time.UTC) }
	ptr := func(v time.Time) *time.Time { return &v }
	zero := time.Time{}
	weekly := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;BYDAY=MO", Mode: recurrenceModel.ModeSchedule}
	after2w := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;INTERVAL=2", Mode: recurrenceModel.ModeCompletion}

	cases := []struct {
		name       string
		r          recurrenceModel.TaskRecurrence
		due, start *time.Time
		done       time.Time
		wantDue    time.Time
		wantStart  *time.Time
	}{
		// Due Mon 5 Oct 17:00, done on time: next Monday.
		{"on schedule", weekly, ptr(at(10, 5, 17)), nil, at(10, 5, 15), at(10, 12, 17), nil},
		// Done early, on Thu 1 Oct: still the Monday after the due date.
		{"done early", weekly, ptr(at(10, 5, 17)), nil, at(10, 1, 10), at(10, 12, 17), nil},
		// Done two weeks late: the next Monday from now, not one already gone.
		{"done late", weekly, ptr(at(10, 5, 17)), nil, at(10, 20, 10), at(10, 26, 17), nil},
		// No due date: from when it was done.
		{"no due date", weekly, ptr(zero), ptr(zero), at(10, 7, 10), at(10, 12, 10), nil},
		// Two weeks after completion, at the time of day it was due.
		{"after completion", after2w, ptr(at(10, 5, 17)), nil, at(10, 8, 11), at(10, 22, 17), nil},
		// A start date keeps its distance before the due date.
		{"start moves too", weekly, ptr(at(10, 5, 17)), ptr(at(10, 2, 9)), at(10, 5, 16), at(10, 12, 17), ptr(at(10, 9, 9))},
	}
	for _, c := range cases {
		due, start := NextOccurrence(c.r, c.due, c.start, c.done)
		if !due.Equal(c.wantDue) {
			t.Errorf("%s: due %v, want %v", c.name, due, c.wantDue)
		}
		if (start == nil) != (c.wantStart == nil) || (start != nil && !start.Equal(*c.wantStart)) {
			t.Errorf("%s: start %v, want %v", c.name, start, c.wantStart)
		}
	}
}

func TestCheckRecurrence(t *testing.T) {
	rule, mode, err := CheckRecurrence("FREQ=WEEKLY;BYDAY=MO", "")
	if err != nil || rule != "FREQ=WEEKLY;BYDAY=MO" || mode != "schedule" {
		t.Fatalf("default mode: %q %q %v", rule, mode, err)
	}
	rule, _, err = CheckRecurrence("FREQ=WEEKLY;INTERVAL=2;BYDAY=MO", "completion")
	if err != nil || rule != "FREQ=WEEKLY;INTERVAL=2" {
		t.Fatalf("completion drops weekdays: %q %v", rule, err)
	}
	var re *RecurrenceError
	for _, c := range [][2]string{{"FREQ=HOURLY", ""}, {"FREQ=DAILY", "sometimes"}} {
		if _, _, err := CheckRecurrence(c[0], c[1]); !errors.As(err, &re) {
			t.Errorf("%v: want a RecurrenceError, got %v", c, err)
		}
	}
}
