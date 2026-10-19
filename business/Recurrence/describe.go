package business

// A rule in words, from its start, as the repeat menu phrases its presets
// (PROGRAM.md D-CAL-7): "Daily", "Weekly on Tuesday", "Monthly on the second
// Tuesday", "Annually on 13 October", "Every weekday", and for the rest a
// sentence of their parts ("Every 2 weeks on Tuesday and Thursday, until 18
// December 2026"), so a rule imported from elsewhere is shown in words too.

import (
	"strconv"
	"strings"
	"time"
)

// Describe is r in words, for a series starting at start in loc.
func Describe(r Rule, start time.Time, loc *time.Location) string {
	local := start.In(loc)
	out := base(r) + detail(r, local)
	switch {
	case r.Count == 1:
		out += ", once"
	case r.Count > 1:
		out += ", " + strconv.Itoa(r.Count) + " times"
	case r.Until != nil:
		until := r.Until.In(loc)
		if r.UntilDate {
			until = r.Until.UTC()
		}
		out += ", until " + until.Format("2 January 2006")
	}
	return out
}

// base is how often: "Weekly", "Every 2 weeks"; "Every weekday" for the
// working week, and "Daily" for a week of every day.
func base(r Rule) string {
	if r.Freq == Weekly && r.Interval == 1 && len(r.ByMonth) == 0 {
		switch {
		case sameDays(r.ByDay, workingWeek):
			return "Every weekday"
		case sameDays(r.ByDay, wholeWeek):
			return "Daily"
		}
	}
	unit := map[Freq]string{Daily: "day", Weekly: "week", Monthly: "month", Yearly: "year"}[r.Freq]
	if r.Interval > 1 {
		return "Every " + strconv.Itoa(r.Interval) + " " + unit + "s"
	}
	return map[Freq]string{Daily: "Daily", Weekly: "Weekly", Monthly: "Monthly", Yearly: "Annually"}[r.Freq]
}

var (
	workingWeek = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	wholeWeek   = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
)

// sameDays is whether BYDAY is exactly these weekdays, with no ordinals.
func sameDays(by []WeekdayNum, days []time.Weekday) bool {
	if len(by) != len(days) {
		return false
	}
	for _, d := range days {
		found := false
		for _, b := range by {
			found = found || (b.Day == d && b.N == 0)
		}
		if !found {
			return false
		}
	}
	return true
}

// detail is on which days: " on Tuesday and Thursday", " on day 16", " on the
// second Tuesday", " on 13 October", " on the last weekday of March".
func detail(r Rule, local time.Time) string {
	if r.Freq == Weekly && r.Interval == 1 && len(r.ByMonth) == 0 && (sameDays(r.ByDay, workingWeek) || sameDays(r.ByDay, wholeWeek)) {
		return ""
	}
	months := ""
	if len(r.ByMonth) > 0 && !(r.Freq == Yearly && len(r.ByDay) == 0 && len(r.ByMonthDay) == 0) {
		var names []string
		for _, m := range sortedMonths(r.ByMonth) {
			names = append(names, m.String())
		}
		months = " in " + list(names)
		if r.Freq == Yearly && len(r.ByDay) > 0 {
			months = " of " + list(names)
		}
	}
	switch r.Freq {
	case Daily:
		if len(r.ByDay) > 0 {
			return " on " + weekdays(r.ByDay) + months
		}
		return months
	case Weekly:
		if len(r.ByDay) == 0 {
			return " on " + local.Weekday().String() + months
		}
		return " on " + weekdays(r.ByDay) + months
	case Monthly:
		return " on " + monthDays(r, local) + months
	default:
		switch {
		case len(r.ByDay) > 0 || len(r.ByMonthDay) > 0:
			return " on " + monthDays(r, local) + months
		case len(r.ByMonth) > 0:
			var names []string
			for _, m := range sortedMonths(r.ByMonth) {
				names = append(names, strconv.Itoa(local.Day())+" "+m.String())
			}
			return " on " + list(names)
		}
		return " on " + local.Format("2 January")
	}
}

// monthDays is which days of a month: the start's, by number, by place
// ("the second Tuesday") or the BYSETPOS'th of the days named ("the last
// weekday").
func monthDays(r Rule, local time.Time) string {
	switch {
	case len(r.BySetPos) == 1 && len(r.ByMonthDay) == 0 && len(r.ByDay) > 0 && allPlain(r.ByDay):
		what := weekdays(r.ByDay)
		if sameDays(r.ByDay, workingWeek) {
			what = "weekday"
		} else if sameDays(r.ByDay, wholeWeek) {
			what = "day"
		}
		return "the " + place(r.BySetPos[0]) + " " + what
	case len(r.ByDay) > 0 && len(r.ByMonthDay) == 0:
		var parts []string
		for _, d := range r.ByDay {
			if d.N == 0 {
				parts = append(parts, "every "+d.Day.String())
			} else {
				parts = append(parts, "the "+place(d.N)+" "+d.Day.String())
			}
		}
		return list(parts)
	case len(r.ByMonthDay) > 0:
		var days []string
		lasts := []string{}
		for _, d := range sortedDays(r.ByMonthDay) {
			if d > 0 {
				days = append(days, strconv.Itoa(d))
			} else {
				lasts = append(lasts, "the "+place(d)+" day")
			}
		}
		var parts []string
		switch len(days) {
		case 0:
		case 1:
			parts = append(parts, "day "+days[0])
		default:
			parts = append(parts, "days "+list(days))
		}
		parts = append(parts, lasts...)
		out := list(parts)
		if len(r.ByDay) > 0 {
			out += " when it is a " + weekdays(r.ByDay)
		}
		return out
	}
	return "day " + strconv.Itoa(local.Day())
}

func allPlain(by []WeekdayNum) bool {
	for _, d := range by {
		if d.N != 0 {
			return false
		}
	}
	return true
}

// weekdays names BYDAY's days in week order: "Tuesday and Thursday".
func weekdays(by []WeekdayNum) string {
	var names []string
	for _, d := range wholeWeek {
		for _, b := range by {
			if b.Day == d && b.N == 0 {
				names = append(names, d.String())
				break
			}
		}
	}
	return list(names)
}

// place is an ordinal: 1 first, 2 second, −1 last, −2 second to last.
func place(n int) string {
	words := map[int]string{1: "first", 2: "second", 3: "third", 4: "fourth", 5: "fifth"}
	switch {
	case n == -1:
		return "last"
	case n < 0:
		if w, ok := words[-n]; ok {
			return w + " to last"
		}
		return strconv.Itoa(-n) + "th to last"
	}
	if w, ok := words[n]; ok {
		return w
	}
	return strconv.Itoa(n) + "th"
}

// list joins words as a sentence does: "a", "a and b", "a, b and c".
func list(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
