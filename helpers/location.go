package helpers

import (
	"strings"
	"time"
)

// Location is the IANA time zone a request named ("Asia/Kolkata"), or UTC
// when it named none or one Go doesn't know. Dates people pick (a due date)
// are their local midnight, so "which day" must be asked in their zone.
func Location(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// DayStart is the midnight that starts t's day in loc. Days are counted in
// the reader's zone, so "today" is the day their calendar shows.
func DayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
