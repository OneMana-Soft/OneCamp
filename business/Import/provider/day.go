package provider

import (
	"strings"
	"time"
)

// Day is a calendar date with no time of day: how most sources keep a due or
// start date (Asana's due_on, Jira's duedate, Linear's dueDate, Basecamp's
// due_on, monday's and Notion's dates, an all-day Todoist due). It is never
// an instant. Parsed as midnight UTC, every imported day read as the day
// before for anyone west of UTC, since the app reads a due date in the
// viewer's own zone. The import writes a Day the way the app writes a day
// someone picks: its midnight where the team is (22 Oct 2026).
type Day struct {
	Year  int
	Month time.Month
	Day   int
}

// ParseDay reads a source's date-only "2006-01-02". ok is false for anything
// else, a date with a time included: that is an instant, not a Day. Pure.
func ParseDay(s string) (d Day, ok bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return Day{}, false
	}
	y, m, dd := t.Date()
	return Day{Year: y, Month: m, Day: dd}, true
}

// IsZero says there is no day. Pure.
func (d Day) IsZero() bool { return d == Day{} }

// At is the day's midnight in loc, as the app writes a picked day. With no
// zone known it is noon UTC, which is the same day everywhere from UTC−12 to
// UTC+11. Pure.
func (d Day) At(loc *time.Location) time.Time {
	if loc == nil {
		return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, time.UTC)
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// String is the day as "2006-01-02", or "" for none. Pure.
func (d Day) String() string {
	if d.IsZero() {
		return ""
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}
