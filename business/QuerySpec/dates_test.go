package business

import (
	"testing"
	"time"
)

// Every compiler reads a date value through Window, so "this week" or "last
// friday" means the same days for search, tasks and rows. Fixed at Saturday
// 17 Oct 2026, 23:30 in Kolkata (still the 17th there, already the 18th in
// Auckland), so the zone is what decides the day.
func TestADateValueMeansTheSameDaysEverywhere(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no zone database:", err)
	}
	now := time.Date(2026, 10, 17, 23, 30, 0, 0, kolkata)
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, kolkata) }
	cases := []struct {
		name     string
		v        Value
		operator string
		ahead    bool
		from, to time.Time
	}{
		{"a day", Value{Text: "2026-10-01"}, OperatorIs, false, d(2026, 10, 1), d(2026, 10, 2)},
		{"a month", Value{Text: "2026-02"}, OperatorIs, false, d(2026, 2, 1), d(2026, 3, 1)},
		{"a year", Value{Text: "2025"}, OperatorIs, false, d(2025, 1, 1), d(2026, 1, 1)},
		{"today", Value{Relative: RelativeToday}, OperatorIs, false, d(2026, 10, 17), d(2026, 10, 18)},
		{"yesterday", Value{Relative: RelativeYesterday}, OperatorIs, false, d(2026, 10, 16), d(2026, 10, 17)},
		{"tomorrow", Value{Relative: RelativeTomorrow}, OperatorIs, false, d(2026, 10, 18), d(2026, 10, 19)},
		{"seven days ago", Value{Relative: RelativeDaysAgo, N: 7}, OperatorIs, false, d(2026, 10, 10), d(2026, 10, 11)},
		{"two months from now", Value{Relative: RelativeMonthsFromNow, N: 2}, OperatorIs, false, d(2026, 12, 17), d(2026, 12, 18)},
		{"a year ago", Value{Relative: RelativeYearsAgo, N: 1}, OperatorIs, false, d(2025, 10, 17), d(2025, 10, 18)},
		{"saturday, looking back, on a saturday", Value{Relative: RelativeWeekday, N: 6}, OperatorIs, false, d(2026, 10, 17), d(2026, 10, 18)},
		{"saturday, looking ahead, on a saturday", Value{Relative: RelativeWeekday, N: 6}, OperatorIs, true, d(2026, 10, 17), d(2026, 10, 18)},
		{"friday, looking back", Value{Relative: RelativeWeekday, N: 5}, OperatorIs, false, d(2026, 10, 16), d(2026, 10, 17)},
		{"friday, looking ahead", Value{Relative: RelativeWeekday, N: 5}, OperatorIs, true, d(2026, 10, 23), d(2026, 10, 24)},
		{"monday, looking back", Value{Relative: RelativeWeekday, N: 1}, OperatorIs, false, d(2026, 10, 12), d(2026, 10, 13)},
		{"monday, looking ahead", Value{Relative: RelativeWeekday, N: 1}, OperatorIs, true, d(2026, 10, 19), d(2026, 10, 20)},
		{"this week starts on monday", Value{Relative: RelativeThisWeek}, OperatorIs, false, d(2026, 10, 12), d(2026, 10, 19)},
		{"last week", Value{Relative: RelativeLastWeek}, OperatorIs, false, d(2026, 10, 5), d(2026, 10, 12)},
		{"next week, the calendar week", Value{Relative: RelativeNextWeek}, OperatorIs, false, d(2026, 10, 19), d(2026, 10, 26)},
		{"next week, rolling", Value{Relative: RelativeNextWeek}, OperatorWithin, false, d(2026, 10, 17), d(2026, 10, 25)},
		{"this month", Value{Relative: RelativeThisMonth}, OperatorIs, false, d(2026, 10, 1), d(2026, 11, 1)},
		{"last month", Value{Relative: RelativeLastMonth}, OperatorIs, false, d(2026, 9, 1), d(2026, 10, 1)},
		{"next month, rolling", Value{Relative: RelativeNextMonth}, OperatorWithin, false, d(2026, 10, 17), d(2026, 11, 18)},
		{"this quarter", Value{Relative: RelativeThisQuarter}, OperatorIs, false, d(2026, 10, 1), d(2027, 1, 1)},
		{"last quarter", Value{Relative: RelativeLastQuarter}, OperatorIs, false, d(2026, 7, 1), d(2026, 10, 1)},
		{"next year", Value{Relative: RelativeNextYear}, OperatorIs, false, d(2027, 1, 1), d(2028, 1, 1)},
		{"march, looking back", Value{Relative: RelativeMonthName, N: 3}, OperatorIs, false, d(2026, 3, 1), d(2026, 4, 1)},
		{"march, looking ahead", Value{Relative: RelativeMonthName, N: 3}, OperatorIs, true, d(2027, 3, 1), d(2027, 4, 1)},
		{"november, looking back", Value{Relative: RelativeMonthName, N: 11}, OperatorIs, false, d(2025, 11, 1), d(2025, 12, 1)},
		{"the past 30 days include today", Value{Relative: RelativePastDays, N: 30}, OperatorWithin, false, d(2026, 9, 17), d(2026, 10, 18)},
		{"the next 7 days include today", Value{Relative: RelativeNextDays, N: 7}, OperatorWithin, false, d(2026, 10, 17), d(2026, 10, 25)},
		{"the past week", Value{Relative: RelativePastWeek}, OperatorWithin, false, d(2026, 10, 10), d(2026, 10, 18)},
		{"the past year", Value{Relative: RelativePastYear}, OperatorWithin, false, d(2025, 10, 17), d(2026, 10, 18)},
	}
	for _, c := range cases {
		from, to, err := c.v.Window(now, kolkata, c.operator, c.ahead)
		if err != nil || !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%s: [%v, %v) %v, want [%v, %v)", c.name, from, to, err, c.from, c.to)
		}
	}

	// The same instant is the 18th in Auckland: the reader's zone decides.
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err == nil {
		from, _, _ := Value{Relative: RelativeToday}.Window(now, auckland, OperatorIs, false)
		if from.Day() != 18 {
			t.Errorf("today in Auckland is the 18th, got %v", from)
		}
	}
	// No zone is UTC.
	if from, _, _ := (Value{Relative: RelativeToday}).Window(now, nil, OperatorIs, false); from.Location() != time.UTC || from.Day() != 17 {
		t.Errorf("no zone: %v", from)
	}
}

// A value that isn't a date says so, and never stands for some other day.
func TestAValueThatIsntADateSaysSo(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for name, v := range map[string]Value{
		"words":           {Text: "soon"},
		"a bad day":       {Text: "2026-02-30"},
		"a short day":     {Text: "2026-2-3"},
		"an unknown name": {Relative: "fortnight"},
		"a weekday of 8":  {Relative: RelativeWeekday, N: 8},
		"a month of 13":   {Relative: RelativeMonthName, N: 13},
		"days ago, minus": {Relative: RelativeDaysAgo, N: -1},
		"too many years":  {Relative: RelativeYearsFromNow, N: maxYears + 1},
		"too many days":   {Relative: RelativePastDays, N: maxDays + 1},
	} {
		if _, _, err := v.Window(now, time.UTC, OperatorIs, false); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
