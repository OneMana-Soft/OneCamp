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
		due, start, _ := NextOccurrence(c.r, c.due, c.start, c.done)
		if !due.Equal(c.wantDue) {
			t.Errorf("%s: due %v, want %v", c.name, due, c.wantDue)
		}
		if (start == nil) != (c.wantStart == nil) || (start != nil && !start.Equal(*c.wantStart)) {
			t.Errorf("%s: start %v, want %v", c.name, start, c.wantStart)
		}
	}
}

// The days in a rule are the setter's days, wherever the store keeps the time.
func TestNextOccurrenceIsWorkedOutInTheRepeatsTimeZone(t *testing.T) {
	india, _ := time.LoadLocation("Asia/Kolkata")
	california, _ := time.LoadLocation("America/Los_Angeles")
	ptr := func(v time.Time) *time.Time { return &v }

	// Every Monday, due at midnight in India: Sunday 18:30 in UTC, where it
	// used to come out on Tuesday.
	weekly := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;BYDAY=MO", Mode: recurrenceModel.ModeSchedule, TimeZone: "Asia/Kolkata"}
	due := time.Date(2026, 10, 5, 0, 0, 0, 0, india).UTC()
	next, _, _ := NextOccurrence(weekly, ptr(due), nil, time.Date(2026, 10, 5, 10, 0, 0, 0, india))
	if want := time.Date(2026, 10, 12, 0, 0, 0, 0, india); !next.Equal(want) {
		t.Errorf("every Monday in India: %v, want %v", next.In(india), want)
	}

	// Two weeks after it's done, done on a Friday evening in California
	// (Saturday in UTC): two weeks from that Friday.
	after := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;INTERVAL=2", Mode: recurrenceModel.ModeCompletion, TimeZone: "America/Los_Angeles"}
	dueCA := time.Date(2026, 10, 9, 9, 0, 0, 0, california).UTC()
	next, _, _ = NextOccurrence(after, ptr(dueCA), nil, time.Date(2026, 10, 9, 20, 0, 0, 0, california))
	if want := time.Date(2026, 10, 23, 9, 0, 0, 0, california); !next.Equal(want) {
		t.Errorf("after completion in California: %v, want %v", next.In(california), want)
	}

	// A repeat set before zones were kept goes on as it did, in UTC.
	old := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;BYDAY=MO", Mode: recurrenceModel.ModeSchedule}
	next, _, _ = NextOccurrence(old, ptr(time.Date(2026, 10, 5, 17, 0, 0, 0, time.UTC)), nil, time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("no zone: %v, want %v", next, want)
	}
}

// A monthly or yearly repeat keeps its day through the months that lack it.
func TestNextOccurrenceKeepsTheDayOfTheMonth(t *testing.T) {
	ptr := func(v time.Time) *time.Time { return &v }
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 17, 0, 0, 0, time.UTC) }
	monthly := recurrenceModel.TaskRecurrence{Rule: "FREQ=MONTHLY", Mode: recurrenceModel.ModeSchedule}
	yearly := recurrenceModel.TaskRecurrence{Rule: "FREQ=YEARLY", Mode: recurrenceModel.ModeSchedule}
	with := func(r recurrenceModel.TaskRecurrence, anchor int) recurrenceModel.TaskRecurrence {
		r.AnchorDay = anchor
		return r
	}
	for _, c := range []struct {
		name       string
		r          recurrenceModel.TaskRecurrence
		due        time.Time
		want       time.Time
		wantAnchor int
	}{
		{"the 31st, into February", with(monthly, 31), day(2026, 1, 31), day(2026, 2, 28), 31},
		// Each copy's due date was the base: Feb 28 led to Mar 28, for good.
		{"the 31st, out of February", with(monthly, 31), day(2026, 2, 28), day(2026, 3, 31), 31},
		{"the 31st, past a 30-day month", with(monthly, 31), day(2026, 4, 30), day(2026, 5, 31), 31},
		// Someone moved the due date to the 15th: the 15th from now on.
		{"a due date moved by hand", with(monthly, 31), day(2026, 1, 15), day(2026, 2, 15), 15},
		{"set before days were kept", with(monthly, 0), day(2026, 3, 30), day(2026, 4, 30), 30},
		{"Feb 29, through a short year", with(yearly, 29), day(2027, 2, 28), day(2028, 2, 29), 29},
	} {
		next, _, anchor := NextOccurrence(c.r, ptr(c.due), nil, c.due.Add(-time.Hour))
		if !next.Equal(c.want) || anchor != c.wantAnchor {
			t.Errorf("%s: %v (day %d), want %v (day %d)", c.name, next, anchor, c.want, c.wantAnchor)
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
