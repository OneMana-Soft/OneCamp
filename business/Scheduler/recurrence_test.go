package business

import (
	"testing"
	"time"
)

func TestNextRun_Daily(t *testing.T) {
	prev := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	from := time.Date(2026, 6, 1, 9, 0, 5, 0, time.UTC) // just fired
	next, err := NextRun("FREQ=DAILY", from, prev)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("daily next = %v, want %v", next, want)
	}
}

func TestNextRun_WeeklyByDay(t *testing.T) {
	// Monday 2026-06-01 10:00; next should be Wednesday 2026-06-03.
	prev := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	from := time.Date(2026, 6, 1, 10, 0, 1, 0, time.UTC)
	next, err := NextRun("FREQ=WEEKLY;BYDAY=MO,WE,FR", from, prev)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if next.Weekday() != time.Wednesday {
		t.Errorf("expected Wednesday, got %v (%v)", next.Weekday(), next)
	}
	if next.Hour() != 10 {
		t.Errorf("expected hour preserved at 10, got %d", next.Hour())
	}
}

func TestNextRun_WeeklyNoByDay(t *testing.T) {
	prev := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC) // Monday
	from := time.Date(2026, 6, 1, 8, 30, 1, 0, time.UTC)
	next, err := NextRun("FREQ=WEEKLY", from, prev)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := prev.AddDate(0, 0, 7)
	if !next.Equal(want) {
		t.Errorf("weekly next = %v, want %v", next, want)
	}
}

func TestNextRun_Invalid(t *testing.T) {
	next, err := NextRun("", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !next.IsZero() {
		t.Errorf("empty rule should yield zero time, got %v", next)
	}
}

func TestPrevOccurrence_Daily(t *testing.T) {
	// 9:00 UTC daily. now = 10:00 → today's 9:00 already passed.
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC) // Wed
	occ, ok := PrevOccurrence("FREQ=DAILY", 9*60, now)
	if !ok {
		t.Fatal("expected an occurrence")
	}
	want := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if !occ.Equal(want) {
		t.Fatalf("occ=%v want=%v", occ, want)
	}

	// now = 08:00 → today's 9:00 not yet; most recent is yesterday 9:00.
	early := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	occ2, _ := PrevOccurrence("FREQ=DAILY", 9*60, early)
	if !occ2.Equal(time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected yesterday 9:00, got %v", occ2)
	}
}

func TestPrevOccurrence_WeeklyByDay(t *testing.T) {
	// Weekdays only at 9:00. 2026-07-04 is a Saturday → most recent is Friday.
	sat := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	occ, ok := PrevOccurrence("FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", 9*60, sat)
	if !ok {
		t.Fatal("expected an occurrence within the week")
	}
	if occ.Weekday() != time.Friday || occ.Hour() != 9 {
		t.Fatalf("expected Friday 9:00, got %v (%s)", occ, occ.Weekday())
	}
}

func TestPrevOccurrence_Unsupported(t *testing.T) {
	if _, ok := PrevOccurrence("FREQ=MONTHLY", 540, time.Now()); ok {
		t.Fatal("MONTHLY should be unsupported here")
	}
	if _, ok := PrevOccurrence("", 540, time.Now()); ok {
		t.Fatal("empty rule should not produce an occurrence")
	}
}

func TestDueForSchedule(t *testing.T) {
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC) // Wed, past 9:00
	occToday := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// No prior run, occurrence passed → due.
	if !DueForSchedule("FREQ=DAILY", 9*60, nil, now) {
		t.Fatal("first run after the fire time should be due")
	}
	// Last run before today's occurrence → due.
	beforeOcc := occToday.Add(-time.Hour)
	if !DueForSchedule("FREQ=DAILY", 9*60, &beforeOcc, now) {
		t.Fatal("a run before today's occurrence should be due")
	}
	// Already ran at/after today's occurrence → not due.
	afterOcc := occToday.Add(time.Minute)
	if DueForSchedule("FREQ=DAILY", 9*60, &afterOcc, now) {
		t.Fatal("a run after today's occurrence must not re-fire")
	}
	// Before the fire time with no allowed occurrence today and ran yesterday
	// at 9:00 → yesterday's occurrence already consumed, today's not due yet.
	early := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	ranYesterday := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if DueForSchedule("FREQ=DAILY", 9*60, &ranYesterday, early) {
		t.Fatal("should not fire before today's scheduled time when yesterday already ran")
	}
}
