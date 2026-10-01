package business

import (
	"testing"
	"time"
)

func TestIntervalMinutesForRule(t *testing.T) {
	cases := map[string]struct {
		mins int
		ok   bool
	}{
		"FREQ=HOURLY":            {60, true},
		"FREQ=HOURLY;INTERVAL=2": {120, true},
		"FREQ=HOURLY;INTERVAL=6": {360, true},
		"freq=hourly;interval=3": {180, true},
		// Interval below the floor clamps up to the hourly minimum.
		"FREQ=HOURLY;INTERVAL=0": {60, true},
		// Fixed-time and unsupported cadences are not interval rules.
		"FREQ=DAILY":           {0, false},
		"FREQ=WEEKLY;BYDAY=MO": {0, false},
		"FREQ=MONTHLY":         {0, false},
		"":                     {0, false},
		"nonsense":             {0, false},
	}
	for rule, want := range cases {
		mins, ok := IntervalMinutesForRule(rule)
		if ok != want.ok || (ok && mins != want.mins) {
			t.Fatalf("IntervalMinutesForRule(%q) = (%d,%v), want (%d,%v)", rule, mins, ok, want.mins, want.ok)
		}
	}
}

func TestValidRecurrenceIncludesInterval(t *testing.T) {
	for _, r := range []string{"FREQ=DAILY", "FREQ=WEEKLY;BYDAY=MO,WE,FR", "FREQ=HOURLY", "FREQ=HOURLY;INTERVAL=2"} {
		if !ValidRecurrence(r) {
			t.Fatalf("%q should be valid", r)
		}
	}
	for _, r := range []string{"FREQ=MONTHLY", "FREQ=YEARLY", "", "nope"} {
		if ValidRecurrence(r) {
			t.Fatalf("%q should be invalid", r)
		}
	}
}

func TestDueForInterval(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	// Never run → due.
	if !DueForInterval(120, nil, now) {
		t.Fatalf("never-run interval routine should be due")
	}
	// Just ran → not due.
	just := now.Add(-30 * time.Minute)
	if DueForInterval(120, &just, now) {
		t.Fatalf("ran 30m ago on a 120m interval should not be due")
	}
	// Interval elapsed exactly → due.
	elapsed := now.Add(-120 * time.Minute)
	if !DueForInterval(120, &elapsed, now) {
		t.Fatalf("interval elapsed should be due")
	}
	// Non-positive interval → never due.
	if DueForInterval(0, nil, now) {
		t.Fatalf("zero interval should never be due")
	}
}
