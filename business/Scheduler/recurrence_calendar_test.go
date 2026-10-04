package business

import (
	"testing"
	"time"
)

func TestNextRunCalendar(t *testing.T) {
	d := func(y int, m time.Month, day, h int) time.Time { return time.Date(y, m, day, h, 0, 0, 0, time.UTC) }
	cases := []struct {
		name, rule string
		from, prev time.Time
		want       time.Time
	}{
		{"31st into February", "FREQ=MONTHLY", d(2026, 1, 31, 9), d(2026, 1, 31, 9), d(2026, 2, 28, 9)},
		{"31st comes back after a short month", "FREQ=MONTHLY", d(2026, 3, 1, 0), d(2026, 1, 31, 9), d(2026, 3, 31, 9)},
		{"leap day, yearly", "FREQ=YEARLY", d(2028, 2, 29, 9), d(2028, 2, 29, 9), d(2029, 2, 28, 9)},
		{"quarterly", "FREQ=MONTHLY;INTERVAL=3", d(2026, 1, 15, 9), d(2026, 1, 15, 9), d(2026, 4, 15, 9)},
		// Mon 5 Oct 2026; every other week on Tue and Thu.
		{"biweekly, same week", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH", d(2026, 10, 6, 9), d(2026, 10, 6, 9), d(2026, 10, 8, 9)},
		{"biweekly, skips a week", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH", d(2026, 10, 8, 9), d(2026, 10, 6, 9), d(2026, 10, 20, 9)},
	}
	for _, c := range cases {
		got, err := NextRun(c.rule, c.from, c.prev)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: got %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

func TestNormaliseCalendarRule(t *testing.T) {
	ok := map[string]string{
		"FREQ=DAILY":                      "FREQ=DAILY",
		"freq=weekly;byday=fr,mo":         "FREQ=WEEKLY;BYDAY=MO,FR",
		"FREQ=MONTHLY;INTERVAL=1":         "FREQ=MONTHLY",
		"INTERVAL=2;FREQ=YEARLY":          "FREQ=YEARLY;INTERVAL=2",
		"FREQ=WEEKLY;INTERVAL=2;BYDAY=TU": "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU",
	}
	for in, want := range ok {
		if got, good := NormaliseCalendarRule(in); !good || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, good, want)
		}
	}
	for _, bad := range []string{"", "FREQ=HOURLY", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;INTERVAL=400",
		"FREQ=DAILY;BYDAY=MO", "FREQ=WEEKLY;BYDAY=XX", "FREQ=WEEKLY;BYDAY=MO,XX", "FREQ=DAILY;COUNT=3"} {
		if _, good := NormaliseCalendarRule(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := WithoutByDay("FREQ=WEEKLY;INTERVAL=2;BYDAY=MO"); got != "FREQ=WEEKLY;INTERVAL=2" {
		t.Errorf("WithoutByDay: %q", got)
	}
}
