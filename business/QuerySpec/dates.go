package business

// What a date value means, the same in every compiler (the search index,
// projects' tasks, tables' rows): the days it stands for, counted in the
// reader's zone, or a field's own zone when it has one fixed
// (design/QUERY_STACK_CONTRACT.md §2 item 6, §4.4).
//
// A compiler turns a condition's operator and the window of its value into
// its own comparison: is and within are [from, to); before is earlier than
// from; after is from to on; on_or_before is earlier than to; on_or_after is
// from from on; between is from the first value's from to the second value's
// to. A saved view keeps relative dates relative, so each run asks again.

import (
	"fmt"
	"time"
)

// The relative dates a value may name (Value.Relative), with N where they
// count.
const (
	// Days.
	RelativeToday         = "today"
	RelativeYesterday     = "yesterday"
	RelativeTomorrow      = "tomorrow"
	RelativeDaysAgo       = "n_days_ago"
	RelativeDaysFromNow   = "n_days_from_now"
	RelativeMonthsAgo     = "n_months_ago"
	RelativeMonthsFromNow = "n_months_from_now"
	RelativeYearsAgo      = "n_years_ago"
	RelativeYearsFromNow  = "n_years_from_now"
	// RelativeWeekday is a weekday, N from 1 (Monday) to 7 (Sunday): the last
	// one, or for a field that looks ahead the next one; today when it is that
	// day.
	RelativeWeekday = "weekday"

	// Calendar periods. Weeks start on Monday.
	RelativeThisWeek    = "this_week"
	RelativeLastWeek    = "last_week"
	RelativeNextWeek    = "next_week"
	RelativeThisMonth   = "this_month"
	RelativeLastMonth   = "last_month"
	RelativeNextMonth   = "next_month"
	RelativeThisQuarter = "this_quarter"
	RelativeLastQuarter = "last_quarter"
	RelativeNextQuarter = "next_quarter"
	RelativeThisYear    = "this_year"
	RelativeLastYear    = "last_year"
	RelativeNextYear    = "next_year"
	// RelativeMonthName is a month, N from 1 to 12: the last one, or for a
	// field that looks ahead the next one; this month when it is that month.
	RelativeMonthName = "month_name"

	// Rolling windows, which include today. With within, next_week,
	// next_month and next_year are rolling windows too, where with any other
	// operator they are calendar periods.
	RelativePastDays  = "past_n_days"
	RelativeNextDays  = "next_n_days"
	RelativePastWeek  = "past_week"
	RelativePastMonth = "past_month"
	RelativePastYear  = "past_year"
)

// The largest N each kind of relative date takes.
const (
	maxDays   = 36600
	maxMonths = 1200
	maxYears  = 100
)

// Window is the days v stands for, [from, to) in zone (UTC when nil), as of
// now: a day, a calendar period, or a rolling window. operator decides
// whether next_week, next_month and next_year are rolling (within) or
// calendar periods. ahead is a field whose dates look forward (due, start).
// An error says what about the value isn't a date.
func (v Value) Window(now time.Time, zone *time.Location, operator string, ahead bool) (from, to time.Time, err error) {
	if zone == nil {
		zone = time.UTC
	}
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, zone)
	}
	today := day(now.In(zone))
	oneDay := func(d time.Time) (time.Time, time.Time, error) { return d, d.AddDate(0, 0, 1), nil }
	counted := func(max int) error {
		if v.N < 0 || v.N > max {
			return fmt.Errorf("%s counts from 0 to %d, not %d", v.Relative, max, v.N)
		}
		return nil
	}
	weekStart := func(d time.Time) time.Time {
		wd := int(d.Weekday())
		if wd == 0 {
			wd = 7
		}
		return d.AddDate(0, 0, 1-wd)
	}
	monthStart := func(d time.Time) time.Time { return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, zone) }
	quarterStart := func(d time.Time) time.Time {
		return time.Date(d.Year(), time.Month((int(d.Month())-1)/3*3+1), 1, 0, 0, 0, 0, zone)
	}
	yearStart := func(d time.Time) time.Time { return time.Date(d.Year(), 1, 1, 0, 0, 0, 0, zone) }

	if v.Relative == "" {
		for _, layout := range []struct {
			format     string
			years, mon int
			days       int
		}{{"2006-01-02", 0, 0, 1}, {"2006-01", 0, 1, 0}, {"2006", 1, 0, 0}} {
			if len(v.Text) != len(layout.format) {
				continue
			}
			if t, perr := time.ParseInLocation(layout.format, v.Text, zone); perr == nil {
				return t, t.AddDate(layout.years, layout.mon, layout.days), nil
			}
		}
		return time.Time{}, time.Time{}, fmt.Errorf("%q isn't a date", v.Text)
	}

	rolling := operator == OperatorWithin
	switch v.Relative {
	case RelativeToday:
		return oneDay(today)
	case RelativeYesterday:
		return oneDay(today.AddDate(0, 0, -1))
	case RelativeTomorrow:
		return oneDay(today.AddDate(0, 0, 1))
	case RelativeDaysAgo, RelativeDaysFromNow:
		if err := counted(maxDays); err != nil {
			return time.Time{}, time.Time{}, err
		}
		if v.Relative == RelativeDaysAgo {
			return oneDay(today.AddDate(0, 0, -v.N))
		}
		return oneDay(today.AddDate(0, 0, v.N))
	case RelativeMonthsAgo, RelativeMonthsFromNow:
		if err := counted(maxMonths); err != nil {
			return time.Time{}, time.Time{}, err
		}
		if v.Relative == RelativeMonthsAgo {
			return oneDay(today.AddDate(0, -v.N, 0))
		}
		return oneDay(today.AddDate(0, v.N, 0))
	case RelativeYearsAgo, RelativeYearsFromNow:
		if err := counted(maxYears); err != nil {
			return time.Time{}, time.Time{}, err
		}
		if v.Relative == RelativeYearsAgo {
			return oneDay(today.AddDate(-v.N, 0, 0))
		}
		return oneDay(today.AddDate(v.N, 0, 0))
	case RelativeWeekday:
		if v.N < 1 || v.N > 7 {
			return time.Time{}, time.Time{}, fmt.Errorf("a weekday is 1 (Monday) to 7 (Sunday), not %d", v.N)
		}
		wd := int(today.Weekday())
		if wd == 0 {
			wd = 7
		}
		if ahead {
			return oneDay(today.AddDate(0, 0, (v.N-wd+7)%7))
		}
		return oneDay(today.AddDate(0, 0, -((wd - v.N + 7) % 7)))

	case RelativeThisWeek, RelativeLastWeek:
		start := weekStart(today)
		if v.Relative == RelativeLastWeek {
			start = start.AddDate(0, 0, -7)
		}
		return start, start.AddDate(0, 0, 7), nil
	case RelativeNextWeek:
		if rolling {
			return today, today.AddDate(0, 0, 8), nil
		}
		start := weekStart(today).AddDate(0, 0, 7)
		return start, start.AddDate(0, 0, 7), nil
	case RelativeThisMonth, RelativeLastMonth:
		start := monthStart(today)
		if v.Relative == RelativeLastMonth {
			start = start.AddDate(0, -1, 0)
		}
		return start, start.AddDate(0, 1, 0), nil
	case RelativeNextMonth:
		if rolling {
			return today, today.AddDate(0, 1, 1), nil
		}
		start := monthStart(today).AddDate(0, 1, 0)
		return start, start.AddDate(0, 1, 0), nil
	case RelativeThisQuarter, RelativeLastQuarter, RelativeNextQuarter:
		start := quarterStart(today)
		switch v.Relative {
		case RelativeLastQuarter:
			start = start.AddDate(0, -3, 0)
		case RelativeNextQuarter:
			start = start.AddDate(0, 3, 0)
		}
		return start, start.AddDate(0, 3, 0), nil
	case RelativeThisYear, RelativeLastYear:
		start := yearStart(today)
		if v.Relative == RelativeLastYear {
			start = start.AddDate(-1, 0, 0)
		}
		return start, start.AddDate(1, 0, 0), nil
	case RelativeNextYear:
		if rolling {
			return today, today.AddDate(1, 0, 1), nil
		}
		start := yearStart(today).AddDate(1, 0, 0)
		return start, start.AddDate(1, 0, 0), nil
	case RelativeMonthName:
		if v.N < 1 || v.N > 12 {
			return time.Time{}, time.Time{}, fmt.Errorf("a month is 1 to 12, not %d", v.N)
		}
		year := today.Year()
		current := int(today.Month())
		if ahead && v.N < current {
			year++
		}
		if !ahead && v.N > current {
			year--
		}
		start := time.Date(year, time.Month(v.N), 1, 0, 0, 0, 0, zone)
		return start, start.AddDate(0, 1, 0), nil

	case RelativePastDays, RelativeNextDays:
		if err := counted(maxDays); err != nil {
			return time.Time{}, time.Time{}, err
		}
		if v.Relative == RelativePastDays {
			return today.AddDate(0, 0, -v.N), today.AddDate(0, 0, 1), nil
		}
		return today, today.AddDate(0, 0, v.N+1), nil
	case RelativePastWeek:
		return today.AddDate(0, 0, -7), today.AddDate(0, 0, 1), nil
	case RelativePastMonth:
		return today.AddDate(0, -1, 0), today.AddDate(0, 0, 1), nil
	case RelativePastYear:
		return today.AddDate(-1, 0, 0), today.AddDate(0, 0, 1), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%q isn't a date this understands", v.Relative)
}
