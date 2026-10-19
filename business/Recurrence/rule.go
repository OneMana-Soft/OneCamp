package business

// A repeat rule: RFC 5545's RRULE, the parts events use. FREQ from DAILY to
// YEARLY, INTERVAL, COUNT or UNTIL, BYDAY (with ordinals for monthly and
// yearly rules: "second Tuesday", "last Friday"), BYMONTHDAY (negative from
// the month's end), BYMONTH, BYSETPOS and WKST. Anything else is refused in
// words rather than half understood: a rule OneCamp expands is one it expands
// exactly as the RFC says.

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// Freq is how often a rule repeats.
type Freq string

const (
	Daily   Freq = "DAILY"
	Weekly  Freq = "WEEKLY"
	Monthly Freq = "MONTHLY"
	Yearly  Freq = "YEARLY"
)

// WeekdayNum is one BYDAY entry: a weekday, and for a monthly or yearly
// rule which of them in the month or year (1 the first, −1 the last; 0
// every one).
type WeekdayNum struct {
	N   int
	Day time.Weekday
}

// Rule is a parsed repeat rule.
type Rule struct {
	Freq     Freq
	Interval int // 1 or more
	// Count, when not 0, is how many times it happens, its start the first.
	Count int
	// Until, when set, is the last moment an instance may start: an instant
	// for a timed series (written in UTC), or with UntilDate a date (its
	// midnight in UTC) for an all-day one.
	Until      *time.Time
	UntilDate  bool
	ByDay      []WeekdayNum
	ByMonthDay []int
	ByMonth    []time.Month
	BySetPos   []int
	WeekStart  time.Weekday
}

// Limits on what a rule may say.
const (
	maxInterval = 1000
	// MaxCount is the most times a counted rule repeats (Google allows 730).
	MaxCount = 1000
)

// ErrRule is a rule that can't be read; its text says why, for people.
type ErrRule struct{ msg string }

func (e *ErrRule) Error() string { return e.msg }

func ruleError(msg string) error { return &ErrRule{msg} }

var dayCodes = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

var codeOfDay = map[time.Weekday]string{time.Sunday: "SU", time.Monday: "MO", time.Tuesday: "TU", time.Wednesday: "WE",
	time.Thursday: "TH", time.Friday: "FR", time.Saturday: "SA"}

// Parse reads an RRULE value ("FREQ=WEEKLY;BYDAY=TU,TH"), with or without
// its "RRULE:" name.
func Parse(text string) (Rule, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "RRULE:"), "rrule:")
	r := Rule{Interval: 1, WeekStart: time.Monday}
	if text == "" {
		return r, ruleError("The repeat rule is empty.")
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(text, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		key = strings.ToUpper(strings.TrimSpace(key))
		value = strings.ToUpper(strings.TrimSpace(value))
		if !ok || value == "" {
			return r, ruleError(`"` + part + `" isn't part of a repeat rule.`)
		}
		if seen[key] {
			return r, ruleError("The repeat rule says " + key + " twice.")
		}
		seen[key] = true
		var err error
		switch key {
		case "FREQ":
			err = r.setFreq(value)
		case "INTERVAL":
			r.Interval, err = boundedInt(value, 1, maxInterval, "INTERVAL")
		case "COUNT":
			r.Count, err = boundedInt(value, 1, MaxCount, "COUNT")
		case "UNTIL":
			err = r.setUntil(value)
		case "BYDAY":
			err = r.setByDay(value)
		case "BYMONTHDAY":
			r.ByMonthDay, err = intList(value, -31, 31, "BYMONTHDAY")
		case "BYMONTH":
			var months []int
			months, err = intList(value, 1, 12, "BYMONTH")
			for _, m := range months {
				r.ByMonth = append(r.ByMonth, time.Month(m))
			}
		case "BYSETPOS":
			r.BySetPos, err = intList(value, -366, 366, "BYSETPOS")
		case "WKST":
			d, ok := dayCodes[value]
			if !ok {
				err = ruleError(`WKST is a day, such as MO.`)
			}
			r.WeekStart = d
		case "BYYEARDAY", "BYWEEKNO", "BYHOUR", "BYMINUTE", "BYSECOND":
			err = ruleError("OneCamp can't repeat events by " + key + " yet.")
		default:
			err = ruleError(`"` + key + `" isn't part of a repeat rule.`)
		}
		if err != nil {
			return r, err
		}
	}
	return r, r.check()
}

func (r *Rule) setFreq(v string) error {
	switch Freq(v) {
	case Daily, Weekly, Monthly, Yearly:
		r.Freq = Freq(v)
		return nil
	case "SECONDLY", "MINUTELY", "HOURLY":
		return ruleError("Events repeat daily at most often.")
	}
	return ruleError(`FREQ is DAILY, WEEKLY, MONTHLY or YEARLY.`)
}

func (r *Rule) setUntil(v string) error {
	if t, err := time.Parse("20060102", v); err == nil {
		r.Until, r.UntilDate = &t, true
		return nil
	}
	if t, err := time.Parse("20060102T150405Z", v); err == nil {
		r.Until = &t
		return nil
	}
	return ruleError("UNTIL is a date (20261218) or a time in UTC (20261218T170000Z).")
}

func (r *Rule) setByDay(v string) error {
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if len(item) < 2 {
			return ruleError(`"` + item + `" isn't a day of the week.`)
		}
		day, ok := dayCodes[item[len(item)-2:]]
		if !ok {
			return ruleError(`"` + item + `" isn't a day of the week.`)
		}
		n := 0
		if num := item[:len(item)-2]; num != "" {
			var err error
			if n, err = strconv.Atoi(num); err != nil || n == 0 || n < -53 || n > 53 {
				return ruleError(`"` + item + `" isn't a day of the week.`)
			}
		}
		r.ByDay = append(r.ByDay, WeekdayNum{N: n, Day: day})
	}
	return nil
}

func boundedInt(v string, lo, hi int, name string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, ruleError(name + " is a whole number from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi) + ".")
	}
	return n, nil
}

func intList(v string, lo, hi int, name string) ([]int, error) {
	var out []int
	for _, item := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil || n < lo || n > hi || n == 0 {
			return nil, ruleError(name + " takes numbers from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi) + ", never 0.")
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// check is the RFC's rules for which parts go together.
func (r *Rule) check() error {
	switch {
	case r.Freq == "":
		return ruleError("The repeat rule needs FREQ.")
	case r.Count > 0 && r.Until != nil:
		return ruleError("A repeat ends after a number of times or on a date, not both.")
	case r.Freq == Weekly && len(r.ByMonthDay) > 0:
		return ruleError("A weekly repeat can't name days of the month.")
	case len(r.BySetPos) > 0 && len(r.ByDay) == 0 && len(r.ByMonthDay) == 0 && len(r.ByMonth) == 0:
		return ruleError("BYSETPOS picks among days another part names.")
	}
	if r.Freq != Monthly && r.Freq != Yearly {
		for _, d := range r.ByDay {
			if d.N != 0 {
				return ruleError("Only a monthly or yearly repeat can say which of a weekday, such as the second Tuesday.")
			}
		}
	}
	if r.Freq == Monthly {
		for _, d := range r.ByDay {
			if d.N < -5 || d.N > 5 {
				return ruleError("A month has at most five of a weekday.")
			}
		}
	}
	return nil
}

// String is the rule in its one written form: the same rule always reads
// the same, so equal rules compare equal.
func (r Rule) String() string {
	parts := []string{"FREQ=" + string(r.Freq)}
	if r.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.Interval))
	}
	if len(r.ByMonth) > 0 {
		months := slices.Clone(r.ByMonth)
		slices.Sort(months)
		var s []string
		for _, m := range months {
			s = append(s, strconv.Itoa(int(m)))
		}
		parts = append(parts, "BYMONTH="+strings.Join(s, ","))
	}
	if len(r.ByMonthDay) > 0 {
		parts = append(parts, "BYMONTHDAY="+joinInts(sortedDays(r.ByMonthDay)))
	}
	if len(r.ByDay) > 0 {
		days := slices.Clone(r.ByDay)
		slices.SortStableFunc(days, func(a, b WeekdayNum) int {
			if da, db := weekdayIndex(a.Day, r.WeekStart), weekdayIndex(b.Day, r.WeekStart); da != db {
				return da - db
			}
			return a.N - b.N
		})
		var s []string
		for _, d := range days {
			item := codeOfDay[d.Day]
			if d.N != 0 {
				item = strconv.Itoa(d.N) + item
			}
			s = append(s, item)
		}
		parts = append(parts, "BYDAY="+strings.Join(s, ","))
	}
	if len(r.BySetPos) > 0 {
		parts = append(parts, "BYSETPOS="+joinInts(sortedDays(r.BySetPos)))
	}
	if r.WeekStart != time.Monday {
		parts = append(parts, "WKST="+codeOfDay[r.WeekStart])
	}
	switch {
	case r.Count > 0:
		parts = append(parts, "COUNT="+strconv.Itoa(r.Count))
	case r.Until != nil && r.UntilDate:
		parts = append(parts, "UNTIL="+r.Until.UTC().Format("20060102"))
	case r.Until != nil:
		parts = append(parts, "UNTIL="+r.Until.UTC().Format("20060102T150405Z"))
	}
	return strings.Join(parts, ";")
}

// sortedDays orders numbers positive first, then negative (1, 15, −1).
func sortedDays(in []int) []int {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b int) int {
		if (a > 0) != (b > 0) {
			if a > 0 {
				return -1
			}
			return 1
		}
		return a - b
	})
	return out
}

func joinInts(in []int) string {
	s := make([]string, len(in))
	for i, n := range in {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// weekdayIndex is a weekday's place in a week that starts on start.
func weekdayIndex(d, start time.Weekday) int { return (int(d) - int(start) + 7) % 7 }
