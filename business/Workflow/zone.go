package business

// A rule's zone: the zone of whoever saved it (trigger_config "tz", sent by
// the web with every save; UTC for a rule saved without one). A due date is a
// day: the web writes the midnight it starts, in the setter's zone. So a
// rule's due conditions read days in its zone, as the task list's Overdue
// group does, and "due in N days" writes that day's midnight there.

import (
	"fmt"
	"strings"
	"time"
)

// zoneNamed is a zone by its IANA name; empty is UTC.
func zoneNamed(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("that time zone isn't one OneCamp knows")
	}
	return loc, nil
}

// calendarDays is how many days from's day is before to's, each read as a
// date in loc (nil is UTC): 0 the same day, -1 the day before.
func calendarDays(from, to time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	fy, fm, fd := from.In(loc).Date()
	ty, tm, td := to.In(loc).Date()
	a := time.Date(fy, fm, fd, 0, 0, 0, 0, time.UTC)
	b := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

// dayStart is the midnight t's day starts at in loc (nil is UTC): a due date
// is a day, written as the web writes one.
func dayStart(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
