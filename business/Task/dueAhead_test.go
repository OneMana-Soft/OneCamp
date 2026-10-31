package business

import (
	"testing"
	"time"

	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
)

// A task that repeats on a schedule, ahead on the calendar (calendar head 9):
// the dates completing it on time will give its next ones, from the due date
// it has now, within the stretch asked, never the task itself.
func TestDueAhead(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	weekly := recurrenceModel.TaskRecurrence{Rule: "FREQ=WEEKLY;BYDAY=MO", Mode: recurrenceModel.ModeSchedule}
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) // a Tuesday
	dates := func(ts []time.Time) []string {
		out := make([]string, len(ts))
		for i, x := range ts {
			out[i] = x.UTC().Format("Mon 2 Jan 15:04")
		}
		return out
	}
	same := func(t *testing.T, got []time.Time, want ...string) {
		t.Helper()
		g := dates(got)
		if len(g) != len(want) {
			t.Fatalf("got %v, want %v", g, want)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("got %v, want %v", g, want)
			}
		}
	}

	t.Run("each Monday after the one it is due, within the stretch", func(t *testing.T) {
		due := day(2026, 11, 2)
		same(t, DueAhead(weekly, &due, now, day(2026, 11, 1), day(2026, 11, 30), 100), "Mon 9 Nov 00:00", "Mon 16 Nov 00:00", "Mon 23 Nov 00:00")
	})
	t.Run("the end of the stretch is not in it", func(t *testing.T) {
		due := day(2026, 11, 2)
		same(t, DueAhead(weekly, &due, now, day(2026, 11, 1), day(2026, 11, 16), 100), "Mon 9 Nov 00:00")
	})
	t.Run("an overdue one counts from now, as one done late does", func(t *testing.T) {
		due := day(2026, 10, 12)
		same(t, DueAhead(weekly, &due, now, day(2026, 10, 1), day(2026, 11, 10), 100), "Mon 2 Nov 00:00", "Mon 9 Nov 00:00")
	})
	t.Run("a stretch weeks ahead walks there", func(t *testing.T) {
		due := day(2026, 11, 2)
		same(t, DueAhead(weekly, &due, now, day(2027, 1, 1), day(2027, 1, 12), 100), "Mon 4 Jan 00:00", "Mon 11 Jan 00:00")
	})
	t.Run("a monthly one on the 31st keeps its day", func(t *testing.T) {
		monthly := recurrenceModel.TaskRecurrence{Rule: "FREQ=MONTHLY", Mode: recurrenceModel.ModeSchedule, AnchorDay: 31}
		due := day(2026, 10, 31)
		same(t, DueAhead(monthly, &due, now, day(2026, 11, 1), day(2027, 2, 1), 100), "Mon 30 Nov 00:00", "Thu 31 Dec 00:00", "Sun 31 Jan 00:00")
	})
	t.Run("days in its own zone: Monday in India", func(t *testing.T) {
		india := weekly
		india.TimeZone = "Asia/Kolkata"
		kolkata, _ := time.LoadLocation("Asia/Kolkata")
		due := time.Date(2026, 11, 2, 0, 0, 0, 0, kolkata) // Sunday 18:30 UTC
		got := DueAhead(india, &due, now, day(2026, 11, 1), day(2026, 11, 20), 100)
		if len(got) != 2 {
			t.Fatalf("got %v", dates(got))
		}
		for _, x := range got {
			if in := x.In(kolkata); in.Weekday() != time.Monday || in.Hour() != 0 {
				t.Fatalf("%v is %v in India", x, in)
			}
		}
	})
	t.Run("at most max", func(t *testing.T) {
		daily := recurrenceModel.TaskRecurrence{Rule: "FREQ=DAILY", Mode: recurrenceModel.ModeSchedule}
		due := day(2026, 10, 28)
		if got := DueAhead(daily, &due, now, day(2026, 10, 1), day(2026, 12, 1), 5); len(got) != 5 {
			t.Fatalf("got %d dates", len(got))
		}
	})
	t.Run("none for a repeat after completion, a task with no due date, or no stretch", func(t *testing.T) {
		due := day(2026, 11, 2)
		after := weekly
		after.Mode = recurrenceModel.ModeCompletion
		zero := time.Time{}
		for name, got := range map[string][]time.Time{
			"after completion": DueAhead(after, &due, now, day(2026, 11, 1), day(2026, 12, 1), 100),
			"no due date":      DueAhead(weekly, nil, now, day(2026, 11, 1), day(2026, 12, 1), 100),
			"zero due date":    DueAhead(weekly, &zero, now, day(2026, 11, 1), day(2026, 12, 1), 100),
			"no stretch":       DueAhead(weekly, &due, now, day(2026, 12, 1), day(2026, 11, 1), 100),
			"no rule":          DueAhead(recurrenceModel.TaskRecurrence{Mode: recurrenceModel.ModeSchedule}, &due, now, day(2026, 11, 1), day(2026, 12, 1), 100),
		} {
			if len(got) != 0 {
				t.Errorf("%s: got %v", name, dates(got))
			}
		}
	})
}
