package business

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParsedWhen is the result of parsing a natural-language "when" phrase.
type ParsedWhen struct {
	At         time.Time // first fire time
	Recurrence string    // RRULE-lite, empty for one-shot
	Display    string    // human-readable echo, e.g. "tomorrow at 9:00 AM"
}

var (
	reInDuration = regexp.MustCompile(`(?i)^in\s+(\d+)\s*(min|mins|minute|minutes|hour|hours|hr|hrs|day|days|week|weeks|month|months)$`)
	reAtTime     = regexp.MustCompile(`(?i)\bat\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b`)
	reEveryDay   = regexp.MustCompile(`(?i)^every\s+(day|morning|monday|tuesday|wednesday|thursday|friday|saturday|sunday|weekday|weekdays)\b`)
)

// ParseWhen turns a phrase like "in 20 minutes", "tomorrow at 9am",
// "every monday at 10:00", "next friday" into a fire time + optional
// recurrence, evaluated in the user's timezone. Returns an error the caller
// can surface as a usage hint when nothing parses.
func ParseWhen(phrase string, tz string) (*ParsedWhen, error) {
	loc := loadLocation(tz)
	now := time.Now().In(loc)
	p := strings.TrimSpace(strings.ToLower(phrase))
	if p == "" {
		return nil, fmt.Errorf("no time given")
	}

	// "in N <unit>"
	if m := reInDuration.FindStringSubmatch(p); m != nil {
		n, _ := strconv.Atoi(m[1])
		at := addUnit(now, n, m[2])
		return &ParsedWhen{At: at, Display: humanize(at, loc)}, nil
	}

	// "every <day|weekday> [at TIME]" → recurring
	if m := reEveryDay.FindStringSubmatch(p); m != nil {
		hour, min := 9, 0 // default 9:00am
		if tm := reAtTime.FindStringSubmatch(p); tm != nil {
			hour, min = parseClock(tm)
		}
		rule, firstDay := recurrenceForEvery(m[1])
		at := nextDateAtTime(now, firstDay, hour, min, loc)
		return &ParsedWhen{At: at, Recurrence: rule, Display: "every " + m[1] + " at " + clockString(hour, min)}, nil
	}

	// Day anchors with optional "at TIME".
	hour, min, hasTime := 9, 0, false
	if tm := reAtTime.FindStringSubmatch(p); tm != nil {
		hour, min = parseClock(tm)
		hasTime = true
	}

	switch {
	case strings.HasPrefix(p, "today"):
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
		if !at.After(now) {
			at = at.Add(time.Hour) // "today" already past → nudge an hour out
		}
		return &ParsedWhen{At: at, Display: humanize(at, loc)}, nil

	case strings.HasPrefix(p, "tomorrow"):
		d := now.AddDate(0, 0, 1)
		at := time.Date(d.Year(), d.Month(), d.Day(), hour, min, 0, 0, loc)
		return &ParsedWhen{At: at, Display: humanize(at, loc)}, nil

	case strings.HasPrefix(p, "next ") || isWeekday(strings.TrimPrefix(p, "on ")):
		wd, ok := weekdayFromText(p)
		if ok {
			at := nextWeekdayAtTime(now, wd, hour, min, loc)
			return &ParsedWhen{At: at, Display: humanize(at, loc)}, nil
		}
	}

	// Bare "at TIME" → today if future, else tomorrow.
	if hasTime {
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return &ParsedWhen{At: at, Display: humanize(at, loc)}, nil
	}

	return nil, fmt.Errorf("couldn't understand the time")
}

func loadLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

func addUnit(t time.Time, n int, unit string) time.Time {
	switch {
	case strings.HasPrefix(unit, "min"):
		return t.Add(time.Duration(n) * time.Minute)
	case strings.HasPrefix(unit, "hour"), strings.HasPrefix(unit, "hr"):
		return t.Add(time.Duration(n) * time.Hour)
	case strings.HasPrefix(unit, "day"):
		return t.AddDate(0, 0, n)
	case strings.HasPrefix(unit, "week"):
		return t.AddDate(0, 0, 7*n)
	case strings.HasPrefix(unit, "month"):
		return t.AddDate(0, n, 0)
	}
	return t.Add(time.Duration(n) * time.Minute)
}

// parseClock reads hour/minute from an "at TIME" regex match group.
func parseClock(m []string) (int, int) {
	hour, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	ampm := strings.ToLower(m[3])
	if ampm == "pm" && hour < 12 {
		hour += 12
	}
	if ampm == "am" && hour == 12 {
		hour = 0
	}
	if hour > 23 {
		hour = 23
	}
	if min > 59 {
		min = 59
	}
	return hour, min
}

func clockString(hour, min int) string {
	ampm := "AM"
	h := hour
	if h >= 12 {
		ampm = "PM"
	}
	if h == 0 {
		h = 12
	} else if h > 12 {
		h -= 12
	}
	return fmt.Sprintf("%d:%02d %s", h, min, ampm)
}

func humanize(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon, Jan 2 at 3:04 PM")
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

func isWeekday(s string) bool {
	for name := range weekdayNames {
		if strings.HasPrefix(s, name) {
			return true
		}
	}
	return false
}

func weekdayFromText(p string) (time.Weekday, bool) {
	for name, wd := range weekdayNames {
		if strings.Contains(p, name) {
			return wd, true
		}
	}
	return time.Sunday, false
}

func nextWeekdayAtTime(now time.Time, wd time.Weekday, hour, min int, loc *time.Location) time.Time {
	d := now
	for i := 0; i < 8; i++ {
		cand := time.Date(d.Year(), d.Month(), d.Day(), hour, min, 0, 0, loc)
		if cand.After(now) && cand.Weekday() == wd {
			return cand
		}
		d = d.AddDate(0, 0, 1)
	}
	return now.AddDate(0, 0, 7)
}

func nextDateAtTime(now time.Time, firstWeekday time.Weekday, hour, min int, loc *time.Location) time.Time {
	if firstWeekday < 0 {
		// daily
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	return nextWeekdayAtTime(now, firstWeekday, hour, min, loc)
}

// recurrenceForEvery maps "monday"/"weekday"/"day" to an RRULE-lite + the
// weekday to anchor the first occurrence (-1 = daily, no specific weekday).
func recurrenceForEvery(token string) (string, time.Weekday) {
	switch token {
	case "day", "morning":
		return "FREQ=DAILY", time.Weekday(-1)
	case "weekday", "weekdays":
		return "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", time.Monday
	default:
		if wd, ok := weekdayNames[token]; ok {
			return "FREQ=WEEKLY;BYDAY=" + bydayCode(wd), wd
		}
	}
	return "FREQ=DAILY", time.Weekday(-1)
}

func bydayCode(wd time.Weekday) string {
	codes := []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
	return codes[int(wd)]
}
