package business

// Expanding a rule into the times it happens, in the series' own zone: each
// instance is a date and the start's time of day there, so a 09:15 London
// stand-up is 09:15 in March and in November (RFC 5545 §3.3.10).
//
// Where the clocks change:
//   - a time that doesn't exist that day (02:30 when the clocks go forward at
//     02:00) is read with the offset before the gap, so it falls an hour later
//     (03:30), as the RFC says (§3.3.5);
//   - a time that happens twice (01:30 when they go back) is its first.
//
// An all-day series is dates: its start is a date's midnight in UTC, and so
// is every instance.

import (
	"slices"
	"time"
)

// Options change how a rule expands.
type Options struct {
	// Clamp keeps a monthly or yearly repeat on its day where the month is
	// too short, taking the month's last day (the 31st on 30 April, 29
	// February on the 28th): how task repeats count (business/Scheduler
	// NextRunOnDay). Without it, as RFC 5545 says and events do, such a
	// month has no instance.
	Clamp bool
	// Exdates are instances left out (EXDATE); Rdates are more (RDATE).
	Exdates, Rdates []time.Time
	// Limit is the most instances one expansion gives; 0 is DefaultLimit.
	Limit int
}

// DefaultLimit is the most instances an expansion gives unless told.
const DefaultLimit = 5000

// maxPeriods bounds the periods one expansion walks: a rule that names a day
// no period has (BYMONTHDAY=30;BYMONTH=2) ends there, never in a loop.
const maxPeriods = 200000

// Expand is the starts of the instances of r, a series starting at start in
// loc, that fall from from to to, both counted in, earliest first. The start
// is always the first instance, whether or not the rule names it.
func Expand(r Rule, start time.Time, loc *time.Location, from, to time.Time, o Options) []time.Time {
	limit := o.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	var out []time.Time
	add := func(t time.Time) {
		if !t.Before(from) && !t.After(to) && len(out) < limit {
			out = append(out, t)
		}
	}
	walk(r, start, loc, from, to, o, func(t time.Time) bool {
		add(t)
		return len(out) < limit
	})
	// RDATEs join the set whatever the rule's own end (RFC 5545 §3.8.5.2).
	for _, d := range o.Rdates {
		add(d)
	}
	out = without(out, o.Exdates)
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
}

// without is ts less the times in ex.
func without(ts, ex []time.Time) []time.Time {
	if len(ex) == 0 {
		return ts
	}
	return slices.DeleteFunc(ts, func(t time.Time) bool {
		return slices.ContainsFunc(ex, func(e time.Time) bool { return e.Equal(t) })
	})
}

// Next is the first instance of r strictly after after, and whether there is
// one.
func Next(r Rule, start time.Time, loc *time.Location, after time.Time, o Options) (time.Time, bool) {
	o.Limit = 1
	got := Expand(r, start, loc, after.Add(time.Nanosecond), farFuture, o)
	if len(got) == 0 {
		return time.Time{}, false
	}
	return got[0], true
}

// farFuture is where an open-ended search stops.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Last is the last instance's start of r, and false when it never ends: for
// a counted rule its last time, for one with an end date that date's last
// moment or the last rdate, whichever is later.
func Last(r Rule, start time.Time, loc *time.Location, o Options) (time.Time, bool) {
	var last time.Time
	switch {
	case r.Count > 0:
		o.Limit = MaxCount + len(o.Rdates)
		all := Expand(r, start, loc, start, farFuture, o)
		if len(all) > 0 {
			last = all[len(all)-1]
		}
	case r.Until != nil:
		last = untilOf(r, loc)
	default:
		return time.Time{}, false
	}
	for _, d := range o.Rdates {
		if d.After(last) {
			last = d
		}
	}
	return last, true
}

// Before is r ending before at: the series' instances up to at, for "this and
// following", which starts a new series at at. A counted rule keeps the count
// of instances before at; any other ends the day before at for an all-day
// series (UNTIL a date, as the RFC wants with a date start), else a second
// before at.
func Before(r Rule, start time.Time, loc *time.Location, at time.Time, allDay bool) Rule {
	cut := r
	if r.Count > 0 {
		cut.Count = max(len(Expand(r, start, loc, start, at.Add(-time.Nanosecond), Options{Limit: MaxCount})), 1)
		return cut
	}
	until, isDate := at.Add(-time.Second).UTC(), false
	if allDay {
		until, isDate = at.UTC().AddDate(0, 0, -1), true
	}
	cut.Until, cut.UntilDate = &until, isDate
	return cut
}

// From is r for a new series starting at at, cut from one starting at start:
// a counted rule keeps the count still to come.
func From(r Rule, start time.Time, loc *time.Location, at time.Time) Rule {
	rest := r
	if r.Count > 0 {
		before := len(Expand(r, start, loc, start, at.Add(-time.Nanosecond), Options{Limit: MaxCount}))
		rest.Count = max(r.Count-before, 1)
	}
	return rest
}

// Shift is r for a series whose start moved by days: the weekdays and the
// days of the month it names move with it, so a weekly stand-up on Tuesdays
// dragged to a Wednesday repeats on Wednesdays. An ordinal stays ("the second
// Tuesday" becomes "the second Wednesday"), and a day counted from the end of
// the month stays as it is.
func Shift(r Rule, days int) Rule {
	if days == 0 {
		return r
	}
	out := r
	if len(r.ByDay) > 0 {
		out.ByDay = make([]WeekdayNum, 0, len(r.ByDay))
		for _, d := range r.ByDay {
			n := WeekdayNum{N: d.N, Day: time.Weekday(((int(d.Day)+days)%7 + 7) % 7)}
			if !slices.Contains(out.ByDay, n) {
				out.ByDay = append(out.ByDay, n)
			}
		}
		// Monday first, as people list the days of a week.
		slices.SortStableFunc(out.ByDay, func(a, b WeekdayNum) int { return (int(a.Day)+6)%7 - (int(b.Day)+6)%7 })
	}
	if len(r.ByMonthDay) > 0 {
		out.ByMonthDay = make([]int, 0, len(r.ByMonthDay))
		for _, d := range r.ByMonthDay {
			if d > 0 {
				d = ((d-1+days)%31+31)%31 + 1
			}
			if !slices.Contains(out.ByMonthDay, d) {
				out.ByMonthDay = append(out.ByMonthDay, d)
			}
		}
	}
	return out
}

// date is a calendar day.
type date struct {
	y int
	m time.Month
	d int
}

func dateOf(t time.Time) date {
	y, m, d := t.Date()
	return date{y, m, d}
}

func (d date) civil() time.Time { return time.Date(d.y, d.m, d.d, 0, 0, 0, 0, time.UTC) }

func (d date) weekday() time.Weekday { return d.civil().Weekday() }

func daysIn(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }

// wallClock turns dates and a time of day into instants in one zone, walking
// forward: the offset the last instant had is tried first, which is right
// unless a clock change came between, and only then are the zone's offsets
// either side looked up. Going forward, a time the clocks repeat is still
// first met with the offset before the change, so the cache gives its first
// occurrence too.
type wallClock struct {
	loc      *time.Location
	h, mi, s int
	off      int
	have     bool
}

func (c *wallClock) at(d date) time.Time {
	if c.have {
		t := time.Date(d.y, d.m, d.d, c.h, c.mi, c.s, 0, time.UTC).Add(-time.Duration(c.off) * time.Second)
		if _, off := t.In(c.loc).Zone(); off == c.off {
			return t
		}
	}
	t := at(d, c.h, c.mi, c.s, c.loc)
	_, c.off = t.In(c.loc).Zone()
	c.have = true
	return t
}

// at is the instant of a date's time of day in loc, by the RFC's rules for a
// time a clock change skips or repeats.
func at(d date, h, mi, s int, loc *time.Location) time.Time {
	naive := time.Date(d.y, d.m, d.d, h, mi, s, 0, time.UTC)
	_, before := naive.Add(-36 * time.Hour).In(loc).Zone()
	_, after := naive.Add(36 * time.Hour).In(loc).Zone()
	var found []time.Time
	for _, off := range []int{before, after} {
		t := naive.Add(-time.Duration(off) * time.Second)
		l := t.In(loc)
		if l.Year() == d.y && l.Month() == d.m && l.Day() == d.d && l.Hour() == h && l.Minute() == mi && l.Second() == s {
			found = append(found, t)
		}
	}
	if len(found) == 0 {
		// Skipped by the clocks: read with the offset before the gap.
		return naive.Add(-time.Duration(before) * time.Second).UTC()
	}
	return slices.MinFunc(found, func(a, b time.Time) int { return a.Compare(b) }).UTC()
}

// walk calls emit with each instance of r from its start on, earliest first,
// until emit says stop, the rule ends, or an instance falls after to. It
// starts near from when the rule has no count (which must be counted from the
// start).
func walk(r Rule, start time.Time, loc *time.Location, from, to time.Time, o Options, emit func(time.Time) bool) {
	local := start.In(loc)
	first := dateOf(local)
	h, mi, s := local.Clock()
	clock := &wallClock{loc: loc, h: h, mi: mi, s: s}
	count := 0
	until := farFuture
	if r.Until != nil {
		until = untilOf(r, loc)
	}
	ended := func(t time.Time) bool {
		return t.After(to) || t.After(until) || (r.Count > 0 && count >= r.Count)
	}
	// The start is always the first instance.
	if ended(start) {
		return
	}
	count++
	if !emit(start.UTC()) {
		return
	}
	p := 0
	if r.Count == 0 && from.After(start) {
		p = max(periodsBetween(r, first, dateOf(from.In(loc)))-2, 0)
		p -= p % r.Interval
	}
	for steps := 0; steps < maxPeriods; steps, p = steps+1, p+r.Interval {
		for _, d := range candidates(r, first, p, o.Clamp) {
			t := clock.at(d)
			if !t.After(start) {
				continue
			}
			if ended(t) {
				return
			}
			count++
			if !emit(t) {
				return
			}
		}
	}
}

// untilOf is the last moment r lets an instance start: its UNTIL, and for an
// UNTIL that is a date, the whole of that day in loc (an all-day series' loc
// is UTC, so its last date). Pure.
func untilOf(r Rule, loc *time.Location) time.Time {
	if !r.UntilDate {
		return *r.Until
	}
	d := dateOf(*r.Until)
	return time.Date(d.y, d.m, d.d, 0, 0, 0, 0, loc).AddDate(0, 0, 1).Add(-time.Nanosecond).UTC()
}

// periodsBetween is how many whole periods of r lie from the first date's to
// the day's. Pure.
func periodsBetween(r Rule, first, day date) int {
	switch r.Freq {
	case Daily:
		return int(day.civil().Sub(first.civil()).Hours() / 24)
	case Weekly:
		a, b := weekStart(first, r.WeekStart), weekStart(day, r.WeekStart)
		return int(b.Sub(a).Hours() / (24 * 7))
	case Monthly:
		return (day.y-first.y)*12 + int(day.m) - int(first.m)
	default:
		return day.y - first.y
	}
}

func weekStart(d date, wkst time.Weekday) time.Time {
	c := d.civil()
	return c.AddDate(0, 0, -weekdayIndex(c.Weekday(), wkst))
}

// candidates are the dates of the rule's p'th period from its start, in
// order, with BYSETPOS applied. Pure.
func candidates(r Rule, first date, p int, clamp bool) []date {
	var ds []date
	switch r.Freq {
	case Daily:
		d := dateOf(first.civil().AddDate(0, 0, p))
		if r.monthOK(d.m) && r.monthDayOK(d) && r.weekdayOK(d.weekday()) {
			ds = []date{d}
		}
	case Weekly:
		week := weekStart(first, r.WeekStart).AddDate(0, 0, 7*p)
		days := []time.Weekday{first.weekday()}
		if len(r.ByDay) > 0 {
			days = nil
			for _, wd := range r.ByDay {
				days = append(days, wd.Day)
			}
		}
		for i := 0; i < 7; i++ {
			d := dateOf(week.AddDate(0, 0, i))
			if slices.Contains(days, d.weekday()) && r.monthOK(d.m) {
				ds = append(ds, d)
			}
		}
	case Monthly:
		m := first.civil().AddDate(0, p, 1-first.d) // the month's first day
		y, mo := m.Year(), m.Month()
		if r.monthOK(mo) {
			ds = r.inMonth(y, mo, first.d, clamp)
		}
	case Yearly:
		y := first.y + p
		ds = r.inYear(y, first, clamp)
	}
	return r.setPos(ds)
}

func (r Rule) monthOK(m time.Month) bool { return len(r.ByMonth) == 0 || slices.Contains(r.ByMonth, m) }

func (r Rule) weekdayOK(wd time.Weekday) bool {
	if len(r.ByDay) == 0 {
		return true
	}
	return slices.ContainsFunc(r.ByDay, func(w WeekdayNum) bool { return w.Day == wd })
}

func (r Rule) monthDayOK(d date) bool {
	if len(r.ByMonthDay) == 0 {
		return true
	}
	n := daysIn(d.y, d.m)
	return slices.ContainsFunc(r.ByMonthDay, func(md int) bool { return md == d.d || md < 0 && n+md+1 == d.d })
}

// inMonth is the rule's dates in one month: its BYMONTHDAY and BYDAY days
// (both when both are named), else the start's day of the month, which a
// short month lacks unless clamp keeps it on the month's last day.
func (r Rule) inMonth(y int, m time.Month, startDay int, clamp bool) []date {
	n := daysIn(y, m)
	if len(r.ByMonthDay) == 0 && len(r.ByDay) == 0 {
		switch {
		case startDay <= n:
			return []date{{y, m, startDay}}
		case clamp:
			return []date{{y, m, n}}
		}
		return nil
	}
	var out []date
	for day := 1; day <= n; day++ {
		d := date{y, m, day}
		if len(r.ByMonthDay) > 0 && !r.monthDayOK(d) {
			continue
		}
		if len(r.ByDay) > 0 && !r.byDayIn(d, monthWeekdayPlace(d), n) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// monthWeekdayPlace is which of its weekday a date is in its month (1 the
// first).
func monthWeekdayPlace(d date) int { return (d.d-1)/7 + 1 }

// byDayIn is whether BYDAY names a date that is the place'th of its weekday
// in a stretch of span days (a month or a year): "2TU" the second Tuesday,
// "-1FR" the last Friday, "MO" any Monday.
func (r Rule) byDayIn(d date, place, span int) bool {
	wd := d.weekday()
	for _, w := range r.ByDay {
		if w.Day != wd {
			continue
		}
		if w.N == 0 || w.N == place {
			return true
		}
		if w.N < 0 {
			// Counted from the end: how many of this weekday are left after it.
			left := (span - dayOfSpan(d, span)) / 7
			if -w.N == left+1 {
				return true
			}
		}
	}
	return false
}

// dayOfSpan is a date's day number within its month (span the month's
// length) or its year.
func dayOfSpan(d date, span int) int {
	if span <= 31 {
		return d.d
	}
	return d.civil().YearDay()
}

// inYear is the rule's dates in one year.
func (r Rule) inYear(y int, first date, clamp bool) []date {
	switch {
	case len(r.ByMonth) > 0:
		// Each month named; with BYMONTH alone, the start's day in each.
		var out []date
		for _, m := range sortedMonths(r.ByMonth) {
			out = append(out, r.inMonth(y, m, first.d, clamp)...)
		}
		return out
	case len(r.ByMonthDay) > 0:
		var out []date
		for m := time.January; m <= time.December; m++ {
			for _, d := range r.inMonth(y, m, first.d, false) {
				if len(r.ByDay) == 0 || r.weekdayOK(d.weekday()) {
					out = append(out, d)
				}
			}
		}
		return out
	case len(r.ByDay) > 0:
		var out []date
		span := 365
		if daysIn(y, time.February) == 29 {
			span = 366
		}
		for day := 1; day <= span; day++ {
			d := dateOf(time.Date(y, time.January, day, 0, 0, 0, 0, time.UTC))
			if r.byDayIn(d, (day-1)/7+1, span) {
				out = append(out, d)
			}
		}
		return out
	default:
		n := daysIn(y, first.m)
		switch {
		case first.d <= n:
			return []date{{y, first.m, first.d}}
		case clamp:
			return []date{{y, first.m, n}}
		}
		return nil
	}
}

func sortedMonths(in []time.Month) []time.Month {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// setPos keeps the BYSETPOS'th of a period's dates (1 the first, −1 the
// last), in order. Pure.
func (r Rule) setPos(ds []date) []date {
	if len(r.BySetPos) == 0 || len(ds) == 0 {
		return ds
	}
	var out []date
	for i, d := range ds {
		for _, pos := range r.BySetPos {
			if pos == i+1 || pos < 0 && len(ds)+pos == i {
				out = append(out, d)
				break
			}
		}
	}
	return out
}
