package Calendar

// Availability: when people are busy, and the times that work. One engine for
// two things:
//
//   - Find a time: the first slots a group of teammates is free, inside
//     working hours, the way Google Calendar suggests times.
//   - Booking pages: the slots an outsider may book with someone, Calendly's
//     way, with buffers around meetings and a minimum notice.
//
// Busy is every OneCamp event a person created or was invited to, plus their
// Google calendars when connected (busy.go). Only start and end leave this
// file: never a title.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Interval is a stretch of time, start inclusive, end exclusive.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// WorkingHours is when someone takes meetings, in their time zone: these
// weekdays (0 Sunday to 6 Saturday) between two wall-clock times, or, with
// Week, each weekday's own ranges, several a day (a person's hours, calendar
// A3: "09:00–12:30 and 13:30–17:00"). Booking pages keep the first shape.
type WorkingHours struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
	TZ    string `json:"tz"`
	// Week is each weekday's ranges, keyed "0" (Sunday) to "6"; a day it
	// leaves out is off. When set it stands for Days, Start and End.
	Week map[string][]Span `json:"week,omitempty"`
}

// Span is a stretch of a day, between two wall-clock times ("09:00", "12:30").
type Span struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// minutes is a span's start and end in minutes after midnight, and whether it reads.
func (s Span) minutes() (int, int, bool) {
	sh, sm, ok1 := clock(s.Start)
	eh, em, ok2 := clock(s.End)
	return sh*60 + sm, eh*60 + em, ok1 && ok2
}

// spansOn is a weekday's ranges in order, read as Check reads them. Pure.
func (w WorkingHours) spansOn(d time.Weekday) []Span {
	if w.Week != nil {
		out := append([]Span(nil), w.Week[strconv.Itoa(int(d))]...)
		sort.Slice(out, func(i, j int) bool { a, _, _ := out[i].minutes(); b, _, _ := out[j].minutes(); return a < b })
		return out
	}
	for _, day := range w.Days {
		if time.Weekday(day) == d {
			return []Span{{Start: w.Start, End: w.End}}
		}
	}
	return nil
}

// AvailabilityError is a request that can't be answered as asked; its text is
// written for the person.
type AvailabilityError struct{ msg string }

func (e *AvailabilityError) Error() string { return e.msg }

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

// Check validates working hours and returns their zone.
func (w WorkingHours) Check() (*time.Location, error) {
	if w.Week != nil {
		return w.checkWeek()
	}
	if len(w.Days) == 0 {
		return nil, &AvailabilityError{"Pick at least one day."}
	}
	for _, d := range w.Days {
		if d < 0 || d > 6 {
			return nil, &AvailabilityError{"Days run from Sunday (0) to Saturday (6)."}
		}
	}
	sh, sm, ok1 := clock(w.Start)
	eh, em, ok2 := clock(w.End)
	if !ok1 || !ok2 {
		return nil, &AvailabilityError{"Hours look like 09:00."}
	}
	if eh*60+em <= sh*60+sm {
		return nil, &AvailabilityError{"The day has to end after it starts."}
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil || w.TZ == "" {
		return nil, &AvailabilityError{"Pick a time zone."}
	}
	return loc, nil
}

// checkWeek validates a week of ranges: days "0" to "6", each range ending
// after it starts, none overlapping another that day, and at least one.
func (w WorkingHours) checkWeek() (*time.Location, error) {
	any := false
	for key := range w.Week {
		d, err := strconv.Atoi(key)
		if err != nil || d < 0 || d > 6 || strconv.Itoa(d) != key {
			return nil, &AvailabilityError{"Days run from Sunday (0) to Saturday (6)."}
		}
		ordered := w.spansOn(time.Weekday(d))
		last := -1
		for _, s := range ordered {
			a, b, ok := s.minutes()
			if !ok {
				return nil, &AvailabilityError{"Hours look like 09:00."}
			}
			if b <= a {
				return nil, &AvailabilityError{"Each range has to end after it starts."}
			}
			if a < last {
				return nil, &AvailabilityError{"A day's ranges can't overlap."}
			}
			last = b
			any = true
		}
	}
	if !any {
		return nil, &AvailabilityError{"Pick at least one day."}
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil || w.TZ == "" {
		return nil, &AvailabilityError{"Pick a time zone."}
	}
	return loc, nil
}

// Windows is the working time between from and to, each range of each day
// in the hours' zone (wall-clock times hold across daylight-saving changes),
// clipped to from and to. Pure.
func Windows(hours WorkingHours, from, to time.Time) []Interval {
	loc, err := hours.Check()
	if err != nil {
		return nil
	}
	var out []Interval
	local := from.In(loc)
	for day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
		for _, s := range hours.spansOn(day.Weekday()) {
			a, b, _ := s.minutes()
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

// MergeBusy sorts intervals and joins any that touch or overlap. Pure.
func MergeBusy(in []Interval) []Interval {
	busy := make([]Interval, 0, len(in))
	for _, b := range in {
		if b.End.After(b.Start) {
			busy = append(busy, b)
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].Start.Before(busy[j].Start) })
	out := busy[:0]
	for _, b := range busy {
		if n := len(out); n > 0 && !b.Start.After(out[n-1].End) {
			if b.End.After(out[n-1].End) {
				out[n-1].End = b.End
			}
			continue
		}
		out = append(out, b)
	}
	return out
}

// SlotRules shape the slots FreeSlots offers.
type SlotRules struct {
	Duration time.Duration
	// Step between possible starts; defaults to 30 minutes, or the duration
	// when that is shorter.
	Step time.Duration
	// Buffer is kept free before and after every meeting.
	Buffer time.Duration
	// Earliest is the first moment a slot may start (now plus notice).
	Earliest time.Time
	// PerDay caps the slots offered on one day; 0 is no cap.
	PerDay int
	// Limit caps the slots offered in all; 0 is no cap.
	Limit int
}

// FreeSlots lists the slots between from and to that fall inside working
// hours and clear every busy interval (merged, sorted) by the buffer. Pure.
// Days are walked in the hours' zone, so a slot is always the same wall-clock
// time, across daylight-saving changes too.
func FreeSlots(busy []Interval, from, to time.Time, hours WorkingHours, r SlotRules) []Interval {
	loc, err := hours.Check()
	if err != nil || r.Duration <= 0 {
		return nil
	}
	step := r.Step
	if step <= 0 {
		step = 30 * time.Minute
		if r.Duration < step {
			step = r.Duration
		}
	}
	earliest := from
	if r.Earliest.After(earliest) {
		earliest = r.Earliest
	}

	var out []Interval
	i := 0 // busy is sorted: never look behind the current slot again
	local := from.In(loc)
	for day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
		n := 0
		for _, span := range hours.spansOn(day.Weekday()) {
			if r.PerDay > 0 && n >= r.PerDay {
				break
			}
			a, b, _ := span.minutes()
			open := time.Date(day.Year(), day.Month(), day.Day(), a/60, a%60, 0, 0, loc)
			shut := time.Date(day.Year(), day.Month(), day.Day(), b/60, b%60, 0, 0, loc)
			for s := open; !s.Add(r.Duration).After(shut); s = s.Add(step) {
				end := s.Add(r.Duration)
				if s.Before(earliest) {
					continue
				}
				if end.After(to) {
					break
				}
				lo, hi := s.Add(-r.Buffer), end.Add(r.Buffer)
				for i < len(busy) && !busy[i].End.After(lo) {
					i++
				}
				clash := false
				for j := i; j < len(busy) && busy[j].Start.Before(hi); j++ {
					if busy[j].End.After(lo) {
						clash = true
						break
					}
				}
				if clash {
					continue
				}
				out = append(out, Interval{Start: s.UTC(), End: end.UTC()})
				n++
				if r.Limit > 0 && len(out) >= r.Limit {
					return out
				}
				if r.PerDay > 0 && n >= r.PerDay {
					break
				}
			}
		}
	}
	return out
}

// BusyFor is when a person is busy between from and to (ReadBusy), and
// whether a Google calendar of theirs couldn't be read: that time is unknown,
// not free, and every caller says so or keeps nothing of it.
func BusyFor(ctx context.Context, userUUID string, from, to time.Time) (Busy, error) {
	return ReadBusy(ctx, userUUID, from, to, BusyOptions{})
}

// PeopleSeenBy splits people into those whose busy times the asker may see,
// by the calendar's one rule (seesWhenOthersAreBusy), and those kept private
// from them, each once and in the order asked. A workspace that keeps when
// members are busy to themselves (calendar.freebusy.members off) keeps it from
// everyone else, and someone who is no member now sees no one else's: the
// asker sees only their own, since even a free slot placed by someone's day
// tells the rest of it. Erring to privacy, a calendar shared with the asker
// doesn't count while the policy is off. Everything that tells when someone
// is busy asks this: their busy times, their status and their hours.
func PeopleSeenBy(ctx context.Context, askerUUID string, people []string) (seen, private []string, err error) {
	// An asker that names no one (uuid.Nil) sees no one else's times.
	asker, _ := uuid.Parse(askerUUID)
	shared, err := stored.seesWhenOthersAreBusy(ctx, asker)
	if err != nil {
		return nil, nil, err
	}
	done := make(map[string]bool, len(people))
	for _, p := range people {
		if done[p] {
			continue
		}
		done[p] = true
		if p == askerUUID || shared {
			seen = append(seen, p)
		} else {
			private = append(private, p)
		}
	}
	return seen, private, nil
}

// BusySeenBy is when each of people is busy between from and to, as the
// asker may see it (PeopleSeenBy): only those are read, and the rest are
// listed in private, once each.
func BusySeenBy(ctx context.Context, askerUUID string, people []string, from, to time.Time) (map[string]Busy, []string, error) {
	seen, private, err := PeopleSeenBy(ctx, askerUUID, people)
	if err != nil {
		return nil, nil, err
	}
	busy := make(map[string]Busy, len(seen))
	for _, p := range seen {
		b, err := BusyFor(ctx, p, from, to)
		if err != nil {
			return nil, nil, err
		}
		busy[p] = b
	}
	return busy, private, nil
}

// FindTimeInput asks for the first times a group is free.
type FindTimeInput struct {
	Participants    []string     `json:"participants"`
	DurationMinutes int          `json:"duration_minutes"`
	From            time.Time    `json:"from"`
	To              time.Time    `json:"to"`
	Hours           WorkingHours `json:"hours"`
}

// FindTimeResult is the suggested slots and, per person, when they're busy.
// Unread is the people whose Google calendar couldn't be read: the slots
// count only what could be, so they may clash there. Private is the people
// whose busy times the workspace keeps to themselves: the slots don't count
// them at all.
type FindTimeResult struct {
	Slots   []Interval            `json:"slots"`
	Busy    map[string][]Interval `json:"busy"`
	Unread  []string              `json:"unread,omitempty"`
	Private []string              `json:"private,omitempty"`
}

const (
	maxFindTimePeople = 20
	maxFindTimeRange  = 14 * 24 * time.Hour
)

// FindTime suggests up to twelve slots, three a day at most, when the
// requester and everyone listed are free.
func FindTime(ctx context.Context, requesterUUID string, in FindTimeInput, now time.Time) (*FindTimeResult, error) {
	people := []string{requesterUUID}
	seen := map[string]bool{requesterUUID: true}
	for _, p := range in.Participants {
		if _, err := uuid.Parse(p); err != nil {
			return nil, &AvailabilityError{"One of those people isn't in this workspace."}
		}
		if !seen[p] {
			seen[p] = true
			people = append(people, p)
		}
	}
	if len(people) > maxFindTimePeople {
		return nil, &AvailabilityError{fmt.Sprintf("Find a time for up to %d people.", maxFindTimePeople)}
	}
	if in.DurationMinutes < 15 || in.DurationMinutes > 8*60 {
		return nil, &AvailabilityError{"Meetings run from 15 minutes to 8 hours."}
	}
	if _, err := in.Hours.Check(); err != nil {
		return nil, err
	}
	from, to := in.From, in.To
	if from.Before(now) {
		from = now
	}
	if !to.After(from) {
		return nil, &AvailabilityError{"Pick a range that ends after it starts."}
	}
	if to.Sub(from) > maxFindTimeRange {
		to = from.Add(maxFindTimeRange)
	}

	busy, private, err := BusySeenBy(ctx, requesterUUID, people, from, to)
	if err != nil {
		return nil, errors.New("couldn't read calendars")
	}
	res := &FindTimeResult{Busy: map[string][]Interval{}, Private: private}
	var all []Interval
	for _, p := range people {
		b, read := busy[p]
		if !read {
			continue
		}
		res.Busy[p] = b.Intervals
		all = append(all, b.Intervals...)
		if b.GoogleUnread {
			res.Unread = append(res.Unread, p)
		}
	}
	// Starts on the quarter hour after now, so the first suggestion isn't 10:07.
	earliest := now.Truncate(15 * time.Minute).Add(15 * time.Minute)
	res.Slots = FreeSlots(MergeBusy(all), from, to, in.Hours, SlotRules{
		Duration: time.Duration(in.DurationMinutes) * time.Minute,
		Earliest: earliest,
		PerDay:   3,
		Limit:    12,
	})
	if res.Slots == nil {
		res.Slots = []Interval{}
	}
	return res, nil
}
