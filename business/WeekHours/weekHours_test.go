package business

import (
	"errors"
	"testing"
	"time"
)

// A range ending at 24:00 runs to the day's last instant; the next day's
// clock starts outside it.
func TestInsideRunsToMidnight(t *testing.T) {
	h := Hours{TZ: "Asia/Kolkata", Week: map[string][]Span{"3": {{Start: "18:00", End: "24:00"}}}}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 4, 17, 59, 59, 999999999, ist): false,
		time.Date(2026, 11, 4, 18, 0, 0, 0, ist):           true,
		time.Date(2026, 11, 4, 23, 59, 59, 999999999, ist): true,
		time.Date(2026, 11, 5, 0, 0, 0, 0, ist):            false,
	} {
		if got := h.Inside(at); got != want {
			t.Errorf("%v: inside %v, want %v", at, got, want)
		}
	}
}

// NextOpen looks only as far as it is asked, and hours that don't check
// open never.
func TestNextOpenLooksOnlySoFar(t *testing.T) {
	mondays := Hours{TZ: "UTC", Week: map[string][]Span{"1": {{Start: "09:00", End: "10:00"}}}}
	saturday := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if at, ok := mondays.NextOpen(saturday, 24*time.Hour); ok {
		t.Errorf("found %v within a day of Saturday noon", at)
	}
	if at, ok := mondays.NextOpen(saturday, 3*24*time.Hour); !ok || !at.Equal(time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v %v, want Monday at nine", at, ok)
	}
	broken := Hours{TZ: "UTC", Week: map[string][]Span{"1": {{Start: "10:00", End: "09:00"}}}}
	if _, ok := broken.NextOpen(saturday, 8*24*time.Hour); ok || broken.Inside(saturday) {
		t.Error("hours that don't check opened")
	}
	var refused *Refused
	if _, err := broken.Check(); !errors.As(err, &refused) || refused.Msg != "Each range has to end after it starts." {
		t.Errorf("check: %v", err)
	}
}
