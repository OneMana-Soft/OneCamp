package business

import "time"

// The resolutions a plan's date is chosen at (linear-15): a day, or a whole
// month, quarter, half or year.
const (
	Day     = "day"
	Month   = "month"
	Quarter = "quarter"
	Half    = "half"
	Year    = "year"
)

// Resolutions is every resolution, finest first.
var Resolutions = []string{Day, Month, Quarter, Half, Year}

func isResolution(r string) bool {
	for _, x := range Resolutions {
		if x == r {
			return true
		}
	}
	return false
}

// Period is the first and the last day of the period at resolution r that
// holds day d, at midnight UTC: a start is kept as its period's first day and
// a target as its last, so a target "Q1 2027" sorts after every day of it.
func Period(d time.Time, r string) (first, last time.Time) {
	y, m, day := d.Date()
	at := func(m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
	switch r {
	case Month:
		first = at(m, 1)
		return first, first.AddDate(0, 1, -1)
	case Quarter:
		first = at(time.Month((int(m)-1)/3*3+1), 1)
		return first, first.AddDate(0, 3, -1)
	case Half:
		first = at(time.Month((int(m)-1)/6*6+1), 1)
		return first, first.AddDate(0, 6, -1)
	case Year:
		return at(time.January, 1), at(time.December, 31)
	}
	first = at(m, day)
	return first, first
}
