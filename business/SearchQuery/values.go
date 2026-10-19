package business

// Reading typed values by family (design/QUERY_STACK_CONTRACT.md §4.4): dates
// as days, periods or rolling windows, numbers in a field's display unit, and
// a checkbox's yes or no.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

var weekdays = map[string]int{
	"monday": 1, "mon": 1, "tuesday": 2, "tue": 2, "tues": 2, "wednesday": 3, "wed": 3,
	"thursday": 4, "thu": 4, "thur": 4, "thurs": 4, "friday": 5, "fri": 5, "saturday": 6, "sat": 6,
	"sunday": 7, "sun": 7,
}

var months = map[string]int{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3, "april": 4, "apr": 4,
	"may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7, "august": 8, "aug": 8,
	"september": 9, "sep": 9, "sept": 9, "october": 10, "oct": 10, "november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

// The named relative dates a value may be, as typed (spaces and hyphens read
// as underscores).
var namedDates = map[string]string{
	"today": querySpec.RelativeToday, "yesterday": querySpec.RelativeYesterday, "tomorrow": querySpec.RelativeTomorrow,
	"this_week": querySpec.RelativeThisWeek, "last_week": querySpec.RelativeLastWeek, "next_week": querySpec.RelativeNextWeek,
	"this_month": querySpec.RelativeThisMonth, "last_month": querySpec.RelativeLastMonth, "next_month": querySpec.RelativeNextMonth,
	"this_quarter": querySpec.RelativeThisQuarter, "last_quarter": querySpec.RelativeLastQuarter, "next_quarter": querySpec.RelativeNextQuarter,
	"this_year": querySpec.RelativeThisYear, "last_year": querySpec.RelativeLastYear, "next_year": querySpec.RelativeNextYear,
	"past_week": querySpec.RelativePastWeek, "past_month": querySpec.RelativePastMonth, "past_year": querySpec.RelativePastYear,
}

var (
	rollingDays = regexp.MustCompile(`^(past|next)_(\d{1,5})_days?$`)
	offsetDate  = regexp.MustCompile(`^([+-])(\d{1,5})([dwmy])$`)
	dayDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	monthDate   = regexp.MustCompile(`^\d{4}-\d{2}$`)
	yearDate    = regexp.MustCompile(`^\d{4}$`)
)

// errNotADate is a value that isn't a date.
type errNotADate struct{ typed string }

func (e errNotADate) Error() string {
	return fmt.Sprintf("“%s” isn't a date. Try a day such as 2026-10-17, or today, last_week or -7d.", e.typed)
}

// readDate is one typed date: a day, a month or a year as written, or a
// relative date. It checks a written date is a real one.
func readDate(typed string) (querySpec.Value, error) {
	t := strings.ToLower(strings.TrimSpace(typed))
	t = strings.NewReplacer(" ", "_", "-", "_").Replace(t)
	written := strings.TrimSpace(typed)
	switch {
	case dayDate.MatchString(written):
		if _, err := time.Parse("2006-01-02", written); err != nil {
			return querySpec.Value{}, errNotADate{typed}
		}
		return querySpec.Value{Text: written}, nil
	case monthDate.MatchString(written):
		if _, err := time.Parse("2006-01", written); err != nil {
			return querySpec.Value{}, errNotADate{typed}
		}
		return querySpec.Value{Text: written}, nil
	case yearDate.MatchString(written):
		return querySpec.Value{Text: written}, nil
	}
	if rel, ok := namedDates[t]; ok {
		return querySpec.Value{Relative: rel}, nil
	}
	if n, ok := weekdays[t]; ok {
		return querySpec.Value{Relative: querySpec.RelativeWeekday, N: n}, nil
	}
	if n, ok := months[t]; ok {
		return querySpec.Value{Relative: querySpec.RelativeMonthName, N: n}, nil
	}
	if m := rollingDays.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[2])
		rel := querySpec.RelativePastDays
		if m[1] == "next" {
			rel = querySpec.RelativeNextDays
		}
		return querySpec.Value{Relative: rel, N: n}, nil
	}
	if m := offsetDate.FindStringSubmatch(strings.ToLower(written)); m != nil {
		n, _ := strconv.Atoi(m[2])
		ago := m[1] == "-"
		var rel string
		switch m[3] {
		case "d", "w":
			if m[3] == "w" {
				n *= 7
			}
			rel = pick(ago, querySpec.RelativeDaysAgo, querySpec.RelativeDaysFromNow)
		case "m":
			rel = pick(ago, querySpec.RelativeMonthsAgo, querySpec.RelativeMonthsFromNow)
		case "y":
			rel = pick(ago, querySpec.RelativeYearsAgo, querySpec.RelativeYearsFromNow)
		}
		return querySpec.Value{Relative: rel, N: n}, nil
	}
	return querySpec.Value{}, errNotADate{typed}
}

func pick(first bool, a, b string) string {
	if first {
		return a
	}
	return b
}

// rolling is a date value that is a rolling window (§4.5: read with within).
func rolling(v querySpec.Value) bool {
	switch v.Relative {
	case querySpec.RelativePastDays, querySpec.RelativeNextDays, querySpec.RelativePastWeek, querySpec.RelativePastMonth, querySpec.RelativePastYear:
		return true
	}
	return false
}

// splitRange is a typed a..b, or false.
func splitRange(typed string) (string, string, bool) {
	a, b, ok := strings.Cut(typed, "..")
	if !ok || strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return "", "", false
	}
	return a, b, true
}

// readNumber is a number in a field's display unit: 1200.50, ₹12.50, 25%.
func readNumber(typed string) (querySpec.Value, error) {
	t := strings.TrimSpace(typed)
	for _, sym := range []string{"₹", "$", "€", "£"} {
		t = strings.TrimPrefix(t, sym)
	}
	t = strings.TrimSuffix(t, "%")
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || t == "" {
		return querySpec.Value{}, fmt.Errorf("“%s” isn't a number", typed)
	}
	return querySpec.Value{Number: &n}, nil
}

// readCheckbox is yes (true) or no (false), or false for neither.
func readCheckbox(typed string) (yes bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(typed)) {
	case "true", "yes", "checked", "on", "1":
		return true, true
	case "false", "no", "unchecked", "off", "0":
		return false, true
	}
	return false, false
}

// wildcard is a text value's operator by its stars: Q4-* starts with, *-draft
// ends with, *plan* contains. ok is false for no stars at either end.
func wildcard(typed string) (operator, text string, ok bool) {
	starts, ends := strings.HasSuffix(typed, "*"), strings.HasPrefix(typed, "*")
	inner := strings.TrimSuffix(strings.TrimPrefix(typed, "*"), "*")
	switch {
	case inner == "":
		return querySpec.OperatorNotEmpty, "", true
	case starts && ends:
		return querySpec.OperatorContains, inner, true
	case starts:
		return querySpec.OperatorStartsWith, inner, true
	case ends:
		return querySpec.OperatorEndsWith, inner, true
	}
	return "", typed, false
}
