package business

import (
	"encoding/json"
	"errors"
	"fmt"
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

// Windows follow dates as FreeSlots does: a weekday off, a Saturday open (a
// project's SLA week may carry them as a booking page's does).
func TestWindowsFollowDates(t *testing.T) {
	var hours Hours
	if err := json.Unmarshal([]byte(`{"days":[1,2,3,4,5],"start":"09:00","end":"17:00","tz":"UTC",
		"dates":{"2026-11-23":[],"2026-11-28":[{"start":"10:00","end":"11:00"}]}}`), &hours); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	var got []string
	for _, w := range Windows(hours, from, from.AddDate(0, 0, 7)) {
		got = append(got, w.Start.Format("Mon 15:04")+"-"+w.End.Format("15:04"))
	}
	want := "[Tue 09:00-17:00 Wed 09:00-17:00 Thu 09:00-17:00 Fri 09:00-17:00 Sat 10:00-11:00]"
	if fmt.Sprint(got) != want {
		t.Errorf("windows:\n got %v\nwant %s", got, want)
	}
}
