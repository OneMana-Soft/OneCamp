package business

import (
	"testing"
	"time"
)

// The week an SLA counts in (linear-24): Monday to Friday, all day, as
// Linear's business days, or a narrower range of hours.
var (
	businessDays = Hours{TZ: "Asia/Kolkata", Days: []int{1, 2, 3, 4, 5}, Start: "00:00", End: "24:00"}
	officeHours  = Hours{TZ: "Europe/London", Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00"}
)

func at(t *testing.T, zone string, y int, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

// A day of business time from Friday evening ends on Monday evening: the
// weekend counts for nothing.
func TestAfterSkipsTheWeekendInTheHoursZone(t *testing.T) {
	from := at(t, "Asia/Kolkata", 2026, time.November, 13, 18, 0) // a Friday
	got, ok := After(businessDays, from, 24*time.Hour)
	if want := at(t, "Asia/Kolkata", 2026, time.November, 16, 18, 0); !ok || !got.Equal(want) {
		t.Errorf("After = %v %v, want %v", got, ok, want)
	}
}

// Inside open hours, business time runs as the clock does.
func TestAfterInsideAnOpenRangeRunsAsTheClock(t *testing.T) {
	from := at(t, "Europe/London", 2026, time.November, 10, 10, 0) // a Tuesday
	got, ok := After(officeHours, from, 3*time.Hour)
	if want := at(t, "Europe/London", 2026, time.November, 10, 13, 0); !ok || !got.Equal(want) {
		t.Errorf("After = %v %v, want %v", got, ok, want)
	}
}

// From closed time, the count starts at the next opening.
func TestAfterFromClosedTimeStartsAtTheNextOpening(t *testing.T) {
	from := at(t, "Europe/London", 2026, time.November, 14, 12, 0) // a Saturday
	got, ok := After(officeHours, from, time.Hour)
	if want := at(t, "Europe/London", 2026, time.November, 16, 10, 0); !ok || !got.Equal(want) {
		t.Errorf("After = %v %v, want %v", got, ok, want)
	}
}

// Wall-clock hours hold across a daylight-saving change: London's clocks go
// forward on Sunday 29 March 2026, and Monday still opens at 09:00 local.
func TestAfterHoldsAcrossADaylightSavingChange(t *testing.T) {
	from := at(t, "Europe/London", 2026, time.March, 27, 16, 0) // Friday, GMT
	got, ok := After(officeHours, from, 2*time.Hour)
	if want := at(t, "Europe/London", 2026, time.March, 30, 10, 0); !ok || !got.Equal(want) {
		t.Errorf("After = %v %v, want %v (BST)", got, ok, want)
	}
}

// A week of business days is five working days.
func TestAfterAWeekOfBusinessDaysIsFiveWorkingDays(t *testing.T) {
	from := at(t, "Asia/Kolkata", 2026, time.November, 11, 9, 30) // a Wednesday
	got, ok := After(businessDays, from, 5*24*time.Hour)
	if want := at(t, "Asia/Kolkata", 2026, time.November, 18, 9, 30); !ok || !got.Equal(want) {
		t.Errorf("After = %v %v, want %v", got, ok, want)
	}
}

// Nothing to count is now; hours that can't be read never end.
func TestAfterOfNothingIsNowAndOfBadHoursIsNever(t *testing.T) {
	from := at(t, "Europe/London", 2026, time.November, 14, 12, 0)
	if got, ok := After(officeHours, from, 0); !ok || !got.Equal(from) {
		t.Errorf("After(0) = %v %v, want %v", got, ok, from)
	}
	if _, ok := After(Hours{TZ: "Nowhere/Else", Days: []int{1}, Start: "09:00", End: "17:00"}, from, time.Hour); ok {
		t.Error("hours in no zone ended a count")
	}
}
