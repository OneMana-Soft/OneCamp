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
// Google Calendar when connected (only timed events not marked "free"). Only
// start and end leave this file: never a title.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Interval is a stretch of time, start inclusive, end exclusive.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// WorkingHours is when someone takes meetings: these weekdays (0 Sunday to
// 6 Saturday), between two wall-clock times in their time zone.
type WorkingHours struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
	TZ    string `json:"tz"`
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
	days := map[time.Weekday]bool{}
	for _, d := range hours.Days {
		days[time.Weekday(d)] = true
	}
	sh, sm, _ := clock(hours.Start)
	eh, em, _ := clock(hours.End)
	earliest := from
	if r.Earliest.After(earliest) {
		earliest = r.Earliest
	}

	var out []Interval
	i := 0 // busy is sorted: never look behind the current slot again
	local := from.In(loc)
	for day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
		if !days[day.Weekday()] {
			continue
		}
		open := time.Date(day.Year(), day.Month(), day.Day(), sh, sm, 0, 0, loc)
		shut := time.Date(day.Year(), day.Month(), day.Day(), eh, em, 0, 0, loc)
		n := 0
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
	return out
}

// BusyFor is when a person is busy between from and to.
func BusyFor(ctx context.Context, userUUID string, from, to time.Time) ([]Interval, error) {
	events, err := domain.GetDgraphEventsByUserId(ctx, userUUID, &from, &to)
	if err != nil {
		return nil, err
	}
	var busy []Interval
	for _, e := range events {
		if e == nil || e.StartTime == nil || e.EndTime == nil {
			continue
		}
		if e.DeletedAt != nil && e.DeletedAt.Year() > 1970 {
			continue
		}
		busy = append(busy, Interval{Start: *e.StartTime, End: *e.EndTime})
	}
	if id, err := uuid.Parse(userUUID); err == nil {
		gcal, err := integrationBusiness.FetchUserGoogleCalendarEvents(ctx, &model.UserInfo{UserPostgresInfo: model.User{Id: id}}, &from, &to)
		if err != nil {
			// A broken Google link must not hide OneCamp's own events.
			helpers.LogErrorWithContext(ctx, "business/Calendar/BusyFor google err: %+v", err)
		}
		for _, g := range gcal {
			if g == nil || g.Transparency == "transparent" || g.Start == nil || g.End == nil || g.Start.DateTime == "" {
				continue
			}
			s, err1 := time.Parse(time.RFC3339, g.Start.DateTime)
			e, err2 := time.Parse(time.RFC3339, g.End.DateTime)
			if err1 == nil && err2 == nil {
				busy = append(busy, Interval{Start: s, End: e})
			}
		}
	}
	return MergeBusy(busy), nil
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
type FindTimeResult struct {
	Slots []Interval            `json:"slots"`
	Busy  map[string][]Interval `json:"busy"`
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

	res := &FindTimeResult{Busy: map[string][]Interval{}}
	var all []Interval
	for _, p := range people {
		b, err := BusyFor(ctx, p, from, to)
		if err != nil {
			return nil, errors.New("couldn't read calendars")
		}
		res.Busy[p] = b
		all = append(all, b...)
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
