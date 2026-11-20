// Package business (WeekHours) is a week of hours in a zone, pure: each
// weekday's ranges of wall-clock time, checked, and the stretches of real
// time they cover. The calendar's working hours and booking pages read it
// (business/Calendar keeps its names as aliases), and so does the schedule
// notifications may reach a person by (notify.schedule, business/Notification).
// It imports only helpers (a zone's next change of clock).
package business

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// Interval is a stretch of time, start inclusive, end exclusive.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Hours is a week of hours in a time zone: these weekdays (0 Sunday to 6
// Saturday) between two wall-clock times, or, with Week, each weekday's own
// ranges, several a day (a person's hours, calendar A3: "09:00–12:30 and
// 13:30–17:00"). Booking pages keep the first shape.
type Hours struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
	TZ    string `json:"tz"`
	// Week is each weekday's ranges, keyed "0" (Sunday) to "6"; a day it
	// leaves out is off. When set it stands for Days, Start and End.
	Week map[string][]Span `json:"week,omitempty"`
	// Dates are days of their own, keyed "2026-12-24" by the hours' zone:
	// that day's ranges in place of its weekday's, none ([]) for a day off;
	// at most MaxDates. Windows and FreeSlots (business/Calendar) read them
	// through SpansFor; Inside and NextOpen don't (the push gate mirrors them
	// in SQL, and a person's schedule keeps no dates).
	Dates map[string][]Span `json:"dates,omitempty"`
}

// MaxDates is how many dates hours may keep ranges of their own for.
const MaxDates = 60

// dateLayout is how a date of the hours' own is written.
const dateLayout = "2006-01-02"

// Span is a stretch of a day, between two wall-clock times ("09:00",
// "12:30"); "24:00" ends one at midnight.
type Span struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Minutes is a span's start and end in minutes after midnight, and whether it reads.
func (s Span) Minutes() (int, int, bool) {
	sh, sm, ok1 := clock(s.Start)
	eh, em, ok2 := clock(s.End)
	return sh*60 + sm, eh*60 + em, ok1 && ok2
}

// SpansOn is a weekday's ranges in order, read as Check reads them. Pure.
func (w Hours) SpansOn(d time.Weekday) []Span {
	if w.Week != nil {
		return ordered(w.Week[strconv.Itoa(int(d))])
	}
	for _, day := range w.Days {
		if time.Weekday(day) == d {
			return []Span{{Start: w.Start, End: w.End}}
		}
	}
	return nil
}

// SpansFor is a day's ranges in order: its date's own when the hours keep
// that date (none for a day off), else its weekday's. day is any time that
// day by the hours' zone. Pure.
func (w Hours) SpansFor(day time.Time) []Span {
	if spans, ok := w.Dates[day.Format(dateLayout)]; ok {
		return ordered(spans)
	}
	return w.SpansOn(day.Weekday())
}

// PastDate is the earliest of the hours' dates before local's day (local is
// now, by the hours' zone), if any. Pure.
func (w Hours) PastDate(local time.Time) (string, bool) {
	today, first := local.Format(dateLayout), ""
	for key := range w.Dates {
		if key < today && (first == "" || key < first) {
			first = key
		}
	}
	return first, first != ""
}

// ordered is a copy of a day's ranges, earliest first.
func ordered(spans []Span) []Span {
	out := append([]Span(nil), spans...)
	sort.Slice(out, func(i, j int) bool { a, _, _ := out[i].Minutes(); b, _, _ := out[j].Minutes(); return a < b })
	return out
}

// Refused is hours, or a request about them, that can't be taken as given;
// its text is written for the person.
type Refused struct{ Msg string }

func (e *Refused) Error() string { return e.Msg }

func clock(s string) (int, int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, 0, false
	}
	return hh, mm, true
}

// Check validates the hours, their dates too, and returns their zone.
func (w Hours) Check() (*time.Location, error) {
	loc, err := w.checkDays()
	if err != nil {
		return nil, err
	}
	if err := w.checkDates(); err != nil {
		return nil, err
	}
	return loc, nil
}

// checkDates validates the hours' dates: at most MaxDates, each a real day
// written YYYY-MM-DD, its ranges read as a weekday's are.
func (w Hours) checkDates() error {
	if len(w.Dates) > MaxDates {
		return &Refused{"Set hours of their own for up to " + strconv.Itoa(MaxDates) + " dates."}
	}
	for key, spans := range w.Dates {
		if d, err := time.Parse(dateLayout, key); err != nil || d.Format(dateLayout) != key {
			return &Refused{"Dates look like 2026-12-24."}
		}
		if err := checkSpans(ordered(spans)); err != nil {
			return err
		}
	}
	return nil
}

// checkSpans validates a day's ranges, earliest first: each reads, ends
// after it starts, and starts no earlier than the one before ends.
func checkSpans(ordered []Span) error {
	last := -1
	for _, s := range ordered {
		a, b, ok := s.Minutes()
		if !ok {
			return &Refused{"Hours look like 09:00."}
		}
		if b <= a {
			return &Refused{"Each range has to end after it starts."}
		}
		if a < last {
			return &Refused{"A day's ranges can't overlap."}
		}
		last = b
	}
	return nil
}

// checkDays validates the week (Week, or Days with Start and End) and its zone.
func (w Hours) checkDays() (*time.Location, error) {
	if w.Week != nil {
		return w.checkWeek()
	}
	if len(w.Days) == 0 {
		return nil, &Refused{"Pick at least one day."}
	}
	for _, d := range w.Days {
		if d < 0 || d > 6 {
			return nil, &Refused{"Days run from Sunday (0) to Saturday (6)."}
		}
	}
	sh, sm, ok1 := clock(w.Start)
	eh, em, ok2 := clock(w.End)
	if !ok1 || !ok2 {
		return nil, &Refused{"Hours look like 09:00."}
	}
	if eh*60+em <= sh*60+sm {
		return nil, &Refused{"The day has to end after it starts."}
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil || w.TZ == "" {
		return nil, &Refused{"Pick a time zone."}
	}
	return loc, nil
}

// checkWeek validates a week of ranges: days "0" to "6", each range ending
// after it starts, none overlapping another that day, and at least one.
func (w Hours) checkWeek() (*time.Location, error) {
	any := false
	for key := range w.Week {
		d, err := strconv.Atoi(key)
		if err != nil || d < 0 || d > 6 || strconv.Itoa(d) != key {
			return nil, &Refused{"Days run from Sunday (0) to Saturday (6)."}
		}
		spans := w.SpansOn(time.Weekday(d))
		if err := checkSpans(spans); err != nil {
			return nil, err
		}
		any = any || len(spans) > 0
	}
	if !any {
		return nil, &Refused{"Pick at least one day."}
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil || w.TZ == "" {
		return nil, &Refused{"Pick a time zone."}
	}
	return loc, nil
}

// Windows is the time the hours cover between from and to, each range of
// each day in the hours' zone (wall-clock times hold across daylight-saving
// changes), clipped to from and to. Pure.
func Windows(hours Hours, from, to time.Time) []Interval {
	loc, err := hours.Check()
	if err != nil {
		return nil
	}
	var out []Interval
	local := from.In(loc)
	for day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
		for _, s := range hours.SpansFor(day) {
			a, b, _ := s.Minutes()
			open := time.Date(day.Year(), day.Month(), day.Day(), a/60, a%60, 0, 0, loc)
			shut := time.Date(day.Year(), day.Month(), day.Day(), b/60, b%60, 0, 0, loc)
			if open.Before(from) {
				open = from
			}
			if shut.After(to) {
				shut = to
			}
			if shut.After(open) {
				out = append(out, Interval{Start: open.UTC(), End: shut.UTC()})
			}
		}
	}
	return out
}

// Inside is whether t falls in one of the hours' ranges by the wall clock in
// their zone: the weekday t's clock shows there, at or after a range's start
// and before its end ("24:00" ends the day). A clock that jumps (daylight
// saving's spring change) skips what it jumps over; one that runs an hour
// again (autumn's) is inside a range both times it shows it. The push gate
// decides the same in SQL (domain/UserFCMToken). Pure.
func (w Hours) Inside(t time.Time) bool {
	loc, err := w.Check()
	if err != nil {
		return false
	}
	return w.insideIn(t.In(loc))
}

// insideIn is Inside for t already in the hours' zone.
func (w Hours) insideIn(local time.Time) bool {
	tod := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second + time.Duration(local.Nanosecond())
	for _, s := range w.SpansOn(local.Weekday()) {
		a, b, ok := s.Minutes()
		if ok && tod >= time.Duration(a)*time.Minute && tod < time.Duration(b)*time.Minute {
			return true
		}
	}
	return false
}

// NextOpen is the first instant at or after t that is Inside the hours,
// looked for until t+within; false when there is none by then. Exact across
// daylight-saving changes: between two changes the zone's clock runs evenly,
// so it reaches a range only at the range's start, or at a change, when it
// may land inside one. Pure.
func (w Hours) NextOpen(t time.Time, within time.Duration) (time.Time, bool) {
	loc, err := w.Check()
	if err != nil {
		return time.Time{}, false
	}
	limit := t.Add(within)
	// Each pass moves on to the zone's next change, always later; the cap
	// is so no pass can ever repeat without end, whatever t is (a focus
	// event's end, the person's own, may lie decades ahead).
	for cur, pass := t, 0; cur.Before(limit) && pass < nextOpenPasses; pass++ {
		local := cur.In(loc)
		if w.insideIn(local) {
			return cur, true
		}
		// The clock runs evenly, offset from UTC by off, until next (the
		// zone's next change, or the limit).
		next := helpers.NextZoneChange(local)
		if next.IsZero() || next.After(limit) {
			next = limit
		}
		_, off := local.Zone()
		wall := func(d time.Time, minutes int) time.Time {
			return d.Add(time.Duration(minutes)*time.Minute - time.Duration(off)*time.Second)
		}
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		for ; wall(day, 0).Before(next); day = day.AddDate(0, 0, 1) {
			for _, s := range w.SpansOn(day.Weekday()) {
				a, _, _ := s.Minutes()
				if at := wall(day, a); !at.Before(cur) && at.Before(next) {
					return at, true
				}
			}
		}
		cur = next
	}
	return time.Time{}, false
}

// nextOpenPasses caps NextOpen's passes, one per change of the zone's clock
// before the limit: a zone changes a few times a year (Go adds a year's
// start or end), so 1024 is centuries of search.
const nextOpenPasses = 1024

// afterChunk is how much calendar time After reads the hours' windows for at
// once, and afterLimit how far ahead it looks before saying the count never
// ends (hours open a minute a week reach a year's target within a century).
const (
	afterChunk = 28 * 24 * time.Hour
	afterLimit = 100 * 366 * 24 * time.Hour
)

// After is the instant d of the hours' open time after from: an SLA's target
// counted in a project's business hours (linear-24), past closed days and
// across daylight-saving changes as Windows reads them. Nothing to count (d
// at most 0) is from itself; false is hours that can't be read, or that don't
// cover d within afterLimit. Pure.
func After(hours Hours, from time.Time, d time.Duration) (time.Time, bool) {
	if d <= 0 {
		return from, true
	}
	if _, err := hours.Check(); err != nil {
		return time.Time{}, false
	}
	left := d
	for cur, end := from, from.Add(afterLimit); cur.Before(end); cur = cur.Add(afterChunk) {
		for _, w := range Windows(hours, cur, cur.Add(afterChunk)) {
			span := w.End.Sub(w.Start)
			if span >= left {
				return w.Start.Add(left).In(from.Location()), true
			}
			left -= span
		}
	}
	return time.Time{}, false
}
