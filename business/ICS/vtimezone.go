package ICS

// A zone's VTIMEZONE (RFC 5545 3.6.5), written from Go's own zone database,
// so an event in Europe/London names the same moments in a calendar that knows
// the zone by name and in one that reads only what it was sent.
//
// A zone that keeps one rule for each change of offset (the last Sunday of
// March, every year) is written as that rule; one whose changes follow no rule
// (Morocco's, around Ramadan), or whose rule ends inside the years an event
// reaches, is written change by change; one that never changes is one
// observance.

import (
	"fmt"
	"strconv"
	"time"
)

// transition is a change of a zone's offset.
type transition struct {
	at       time.Time // the first instant of the new period, in UTC
	from, to int       // the offsets before and after, in seconds east of UTC
	dst      bool      // the period it begins is daylight time
	name     string    // the new period's abbreviation ("BST")
}

// period is what a zone says at an instant.
type period struct {
	off int
	dst bool
}

func periodAt(loc *time.Location, t time.Time) period {
	in := t.In(loc)
	_, off := in.Zone()
	return period{off: off, dst: in.IsDST()}
}

// transitions are loc's changes of offset between start and end, each to the
// second. Pure.
func transitions(loc *time.Location, start, end time.Time) []transition {
	var out []transition
	prev, was := start, periodAt(loc, start)
	for t := start.Add(24 * time.Hour); !t.After(end); t = t.Add(24 * time.Hour) {
		now := periodAt(loc, t)
		if now == was {
			prev = t
			continue
		}
		lo, hi := prev, t // lo is in the old period, hi in the new
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
			if !mid.After(lo) {
				mid = lo.Add(time.Second)
			}
			if periodAt(loc, mid) == was {
				lo = mid
			} else {
				hi = mid
			}
		}
		name, _ := hi.In(loc).Zone()
		out = append(out, transition{at: hi.UTC(), from: was.off, to: now.off, dst: now.dst, name: name})
		prev, was = t, now
	}
	return out
}

// vtimezone is loc's VTIMEZONE for the events from..to reach: from the year
// before the first (so the offset at it is defined) to the year after the
// last. Pure.
func vtimezone(loc *time.Location, from, to time.Time) []string {
	start := time.Date(from.In(loc).Year()-1, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(to.In(loc).Year()+1, 12, 31, 0, 0, 0, 0, time.UTC)
	lines := []string{"BEGIN:VTIMEZONE", "TZID:" + paramText(loc.String())}
	ts := transitions(loc, start, end)
	if len(ts) == 0 {
		name, off := start.In(loc).Zone()
		lines = append(lines, observance("STANDARD", time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), off, off, name, "")...)
		return append(lines, "END:VTIMEZONE")
	}
	// What the zone said before its first change here, so a time before it
	// is defined too.
	first := ts[0]
	beforeName, _ := first.at.Add(-time.Second).In(loc).Zone()
	kind := "STANDARD"
	if periodAt(loc, first.at.Add(-time.Second)).dst {
		kind = "DAYLIGHT"
	}
	lines = append(lines, observance(kind, wallClock(start, first.from), first.from, first.from, beforeName, "")...)
	byShape := map[string][]transition{}
	var shapes []string
	for _, t := range ts {
		key := fmt.Sprintf("%t/%d/%d/%s", t.dst, t.from, t.to, t.name)
		if _, ok := byShape[key]; !ok {
			shapes = append(shapes, key)
		}
		byShape[key] = append(byShape[key], t)
	}
	lastYear := ts[len(ts)-1].at.Year()
	for _, key := range shapes {
		group := byShape[key]
		kind := "STANDARD"
		if group[0].dst {
			kind = "DAYLIGHT"
		}
		if rule, ok := yearlyRule(group, lastYear); ok {
			g := group[0]
			lines = append(lines, observance(kind, wallClock(g.at, g.from), g.from, g.to, g.name, rule)...)
			continue
		}
		for _, g := range group {
			lines = append(lines, observance(kind, wallClock(g.at, g.from), g.from, g.to, g.name, "")...)
		}
	}
	return append(lines, "END:VTIMEZONE")
}

// wallClock is the clock reading at instant t in a zone offset off seconds
// from UTC, as a time whose fields are that reading. Pure.
func wallClock(t time.Time, off int) time.Time {
	return t.UTC().Add(time.Duration(off) * time.Second)
}

var weekdays = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// yearlyRule is the RRULE the changes of one kind keep, when they keep one:
// one a year, every year up to the zone's last change here, each in the same
// month, on the same weekday and wall-clock time, either the same week of the
// month or always its last. Pure.
func yearlyRule(group []transition, lastYear int) (string, bool) {
	if len(group) < 2 {
		return "", false
	}
	first := wallClock(group[0].at, group[0].from)
	allLast, sameWeek := true, true
	week := (first.Day()-1)/7 + 1
	for i, g := range group {
		w := wallClock(g.at, g.from)
		if w.Year() != first.Year()+i || w.Month() != first.Month() || w.Weekday() != first.Weekday() ||
			w.Hour() != first.Hour() || w.Minute() != first.Minute() || w.Second() != first.Second() {
			return "", false
		}
		if w.Day() <= daysIn(w.Year(), w.Month())-7 {
			allLast = false
		}
		if (w.Day()-1)/7+1 != week {
			sameWeek = false
		}
	}
	// A rule that stops before the zone's last change here would run on in a
	// calendar that reads it: written change by change instead.
	if wallClock(group[len(group)-1].at, group[len(group)-1].from).Year() < lastYear-1 {
		return "", false
	}
	day := weekdays[first.Weekday()]
	switch {
	case allLast:
		return "FREQ=YEARLY;BYMONTH=" + strconv.Itoa(int(first.Month())) + ";BYDAY=-1" + day, true
	case sameWeek && week <= 4:
		return "FREQ=YEARLY;BYMONTH=" + strconv.Itoa(int(first.Month())) + ";BYDAY=" + strconv.Itoa(week) + day, true
	}
	return "", false
}

func daysIn(year int, m time.Month) int {
	return time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// observance is one STANDARD or DAYLIGHT part: from its onset (a wall-clock
// reading in the offset before it), the offsets it goes from and to, its
// abbreviation, and its yearly rule when it has one. Pure.
func observance(kind string, onset time.Time, from, to int, name, rule string) []string {
	lines := []string{"BEGIN:" + kind, "DTSTART:" + onset.Format("20060102T150405"),
		"TZOFFSETFROM:" + utcOffset(from), "TZOFFSETTO:" + utcOffset(to)}
	if rule != "" {
		lines = append(lines, "RRULE:"+rule)
	}
	if name != "" {
		lines = append(lines, "TZNAME:"+text(name))
	}
	return append(lines, "END:"+kind)
}

// utcOffset is an offset in seconds as a UTC-OFFSET value: "+0530", "-0330",
// or with seconds when it has them. Pure.
func utcOffset(secs int) string {
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	h, m, s := secs/3600, secs%3600/60, secs%60
	out := fmt.Sprintf("%s%02d%02d", sign, h, m)
	if s != 0 {
		out += fmt.Sprintf("%02d", s)
	}
	return out
}
