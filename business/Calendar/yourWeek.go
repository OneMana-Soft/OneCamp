package Calendar

// Your week (calendar B3, gcal-new-time-insights): where the asker's week
// went. Meetings one to one and with three or more, focus, time on their own,
// the working time left free, invitations still to answer, the usual week's
// meetings beside this one, and the people they meet most. Every plan, and
// only ever the asker's own week: nobody reads another person's.
//
// Overlaps count once: two meetings at the same hour are one hour of
// meetings, and an hour of focus inside a meeting is the meeting's.

import (
	"context"
	"sort"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// MaxWeekSpan is the longest stretch Your week reads at once.
const MaxWeekSpan = 31 * 24 * time.Hour

// usualWeeks is how many weeks before this one make the usual week.
const usualWeeks = 4

// topPeople is how many of the people met most Your week names.
const topPeople = 5

// WeekInsights is where a stretch of the asker's time went, in minutes.
type WeekInsights struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Meetings are timed events with at least one other person, not
	// declined: OneOnOne with exactly one, Group with two or more, as the
	// asker may see the guests: a meeting whose organiser hides its guest
	// list counts its organiser alone, so may be one to one here (CR-68 SF2).
	MeetingMinutes  int `json:"meeting_minutes"`
	OneOnOneMinutes int `json:"one_on_one_minutes"`
	GroupMinutes    int `json:"group_minutes"`
	Meetings        int `json:"meetings"`
	// AverageMeetingMinutes is one meeting's length on average.
	AverageMeetingMinutes int `json:"average_meeting_minutes"`
	FocusMinutes          int `json:"focus_minutes"`
	// OwnMinutes is time blocked with nobody else that isn't focus or away.
	OwnMinutes  int `json:"own_minutes"`
	AwayMinutes int `json:"away_minutes"`
	// WorkingMinutes is the working hours in the stretch, and FreeMinutes
	// those nothing makes busy. HasHours is false when the asker set no
	// working hours: then both are 0.
	HasHours       bool `json:"has_hours"`
	WorkingMinutes int  `json:"working_minutes"`
	FreeMinutes    int  `json:"free_minutes"`
	// ToAnswer is the invitations in the stretch still unanswered.
	ToAnswer int `json:"to_answer"`
	// UsualMeetingMinutes is the meetings of an average stretch as long as
	// this one, over the four before it.
	UsualMeetingMinutes int `json:"usual_meeting_minutes"`
	// UsualCut is a history too long to read whole: the usual week is low.
	UsualCut bool         `json:"usual_cut,omitempty"`
	People   []WeekPerson `json:"people"`
}

// WeekPerson is someone the asker met in the stretch, and for how long.
type WeekPerson struct {
	UUID     string `json:"user_uuid"`
	Name     string `json:"name"`
	Minutes  int    `json:"minutes"`
	Meetings int    `json:"meetings"`
}

// weekItem is one occurrence as Your week counts it: its time, what it is,
// and the other members on it. Pure input.
type weekItem struct {
	Interval
	Kind     EventKind
	Others   []uuid.UUID
	External int // guests by email, who count as people but have no id
	ToAnswer bool
}

const (
	itemMeeting EventKind = "meeting"
	itemOwn     EventKind = "own"
)

// YourWeek is the asker's week from `from` until before `to` (at most
// MaxWeekSpan), and the usual week from the four stretches before it.
func YourWeek(ctx context.Context, who *model.UserInfo, from, to time.Time) (*WeekInsights, error) {
	if !to.After(from) || to.Sub(from) > MaxWeekSpan {
		return nil, inputError("Ask for 31 days or less at a time.")
	}
	return stored.yourWeek(ctx, personOf(who), from, to)
}

func (s *service) yourWeek(ctx context.Context, me uuid.UUID, from, to time.Time) (*WeekInsights, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	span := to.Sub(from)
	before := from.Add(-usualWeeks * span)
	// This stretch and the history are two reads (CR-68 SF1): one read of
	// both, cut at maxInstances by start, would lose the newest rows, which
	// are this stretch. A history past the cut only makes the usual week
	// lower, and says so (UsualCut).
	now, _, err := s.weekItems(ctx, me, from, to)
	if err != nil {
		return nil, err
	}
	earlier, cut, err := s.weekItems(ctx, me, before, from)
	if err != nil {
		return nil, err
	}

	var work []Interval
	hours, err := hoursOfPeople(ctx, []uuid.UUID{me})
	if err != nil {
		return nil, err
	}
	h, hasHours := hours[me]
	if hasHours {
		work = Windows(h, from, to)
	}
	var busy []Interval
	if hasHours {
		b, err := ReadBusy(ctx, me.String(), from, to, BusyOptions{})
		if err != nil {
			return nil, err
		}
		busy = b.Intervals
	}

	out := weekOf(now, from, to, work, busy)
	out.HasHours = hasHours
	out.UsualMeetingMinutes = usualMeetings(earlier, before, from, span)
	out.UsualCut = cut

	// Name only the few people Your week shows, in one read.
	if len(out.People) > 0 && s.people != nil {
		ids := make([]string, 0, len(out.People))
		for _, p := range out.People {
			ids = append(ids, p.UUID)
		}
		named, err := s.people(ctx, ids)
		if err == nil {
			byID := map[string]string{}
			for _, p := range named {
				if p != nil {
					byID[p.Uuid] = p.UserFullName
					if byID[p.Uuid] == "" {
						byID[p.Uuid] = p.UserName
					}
				}
			}
			for i := range out.People {
				out.People[i].Name = byID[out.People[i].UUID]
			}
		}
	}
	return out, nil
}

// weekItems is the items of me's occurrences between from and to, and
// whether the read was cut at maxInstances.
func (s *service) weekItems(ctx context.Context, me uuid.UUID, from, to time.Time) ([]weekItem, bool, error) {
	rows, guests, cut, err := s.rangeRows(ctx, me, from, to)
	if err != nil {
		return nil, false, err
	}
	occ := s.withAnswers(ctx, rows, guests)
	out := make([]weekItem, 0, len(rows))
	for _, inst := range rows {
		if it, ok := weekItemOf(inst, guestsShown(inst.owner(), occ[inst.ref], me), me); ok {
			out = append(out, it)
		}
	}
	return out, cut, nil
}

// weekItemOf is how Your week counts one occurrence for me: false for what
// takes none of my time (all day or a whole day long, see-through, cancelled, declined, in the
// trash, a reminder or a working location). Pure.
func weekItemOf(inst instance, guests []models.Attendee, me uuid.UUID) (weekItem, bool) {
	e := inst.Event
	if e == nil || e.AllDay() || e.StartAt == nil || e.EndAt == nil || !e.EndAt.After(*e.StartAt) {
		return weekItem{}, false
	}
	if e.Status == statusCancelled || e.DeletedAt != nil {
		return weekItem{}, false
	}
	kind := EventKind(e.Kind)
	if kind == EventReminder || kind == EventWorkingLocation {
		return weekItem{}, false
	}
	// A timed event of a whole day or more marks the day, as an all-day one
	// does: a 24-hour "Launch day" isn't a day of meetings. Time off counts.
	if kind != EventAway && e.EndAt.Sub(*e.StartAt) >= 24*time.Hour {
		return weekItem{}, false
	}
	it := weekItem{Interval: Interval{Start: *e.StartAt, End: *e.EndAt}}
	mine := answerOf(guests, models.GuestKey{UserID: &me})
	if mine != nil && mine.Response == responseDeclined {
		return weekItem{}, false
	}
	switch kind {
	case EventFocus, EventAway:
		it.Kind = kind
		return it, true
	}
	if e.Transparency == transparencyTransparent {
		return weekItem{}, false
	}
	seen := map[uuid.UUID]bool{me: true}
	owner := inst.owner().CreatedBy
	if owner != uuid.Nil && !seen[owner] {
		seen[owner] = true
		it.Others = append(it.Others, owner)
	}
	for _, g := range guests {
		if g.Response == responseDeclined {
			continue
		}
		if g.UserID == nil {
			if g.Email != "" {
				it.External++
			}
			continue
		}
		if !seen[*g.UserID] {
			seen[*g.UserID] = true
			it.Others = append(it.Others, *g.UserID)
		}
	}
	it.ToAnswer = mine != nil && mine.Response == responseNeedsAction && owner != me
	if len(it.Others)+it.External == 0 {
		it.Kind = itemOwn
	} else {
		it.Kind = itemMeeting
	}
	return it, true
}

// weekOf counts items between from and to, clipping each to it, every kind's
// time merged so overlaps count once; free is working time nothing makes busy.
// Pure.
func weekOf(items []weekItem, from, to time.Time, work, busy []Interval) *WeekInsights {
	out := &WeekInsights{From: from, To: to, People: []WeekPerson{}}
	clip := func(i Interval) (Interval, bool) {
		if i.Start.Before(from) {
			i.Start = from
		}
		if i.End.After(to) {
			i.End = to
		}
		return i, i.End.After(i.Start)
	}
	var meetings, ones, groups, focus, own, away []Interval
	var meetingTotal time.Duration
	type met struct {
		d time.Duration
		n int
	}
	people := map[uuid.UUID]*met{}
	for _, it := range items {
		c, ok := clip(it.Interval)
		if !ok {
			continue
		}
		switch it.Kind {
		case EventFocus:
			focus = append(focus, c)
		case EventAway:
			away = append(away, c)
		case itemOwn:
			own = append(own, c)
		case itemMeeting:
			meetings = append(meetings, c)
			out.Meetings++
			meetingTotal += c.End.Sub(c.Start)
			if len(it.Others)+it.External == 1 {
				ones = append(ones, c)
			} else {
				groups = append(groups, c)
			}
			for _, p := range it.Others {
				m := people[p]
				if m == nil {
					m = &met{}
					people[p] = m
				}
				m.d += c.End.Sub(c.Start)
				m.n++
			}
			if it.ToAnswer {
				out.ToAnswer++
			}
		}
	}
	meetings = MergeBusy(meetings)
	out.MeetingMinutes = minutesOf(meetings)
	out.OneOnOneMinutes = minutesOf(MergeBusy(ones))
	out.GroupMinutes = minutesOf(MergeBusy(groups))
	// Focus inside a meeting is the meeting's; time on your own inside
	// either is theirs.
	focus = cutAll(MergeBusy(focus), meetings)
	out.FocusMinutes = minutesOf(focus)
	out.OwnMinutes = minutesOf(cutAll(cutAll(MergeBusy(own), meetings), focus))
	out.AwayMinutes = minutesOf(MergeBusy(away))
	if out.Meetings > 0 {
		out.AverageMeetingMinutes = int((meetingTotal / time.Duration(out.Meetings)).Round(time.Minute) / time.Minute)
	}
	work = MergeBusy(work)
	out.WorkingMinutes = minutesOf(work)
	out.FreeMinutes = minutesOf(cutAll(work, MergeBusy(busy)))

	for id, m := range people {
		out.People = append(out.People, WeekPerson{UUID: id.String(), Minutes: int(m.d / time.Minute), Meetings: m.n})
	}
	sort.Slice(out.People, func(i, j int) bool {
		a, b := out.People[i], out.People[j]
		if a.Minutes != b.Minutes {
			return a.Minutes > b.Minutes
		}
		if a.Meetings != b.Meetings {
			return a.Meetings > b.Meetings
		}
		return a.UUID < b.UUID
	})
	if len(out.People) > topPeople {
		out.People = out.People[:topPeople]
	}
	return out
}

// usualMeetings is the meeting minutes of an average stretch of span over
// the usualWeeks stretches from before until from. Pure.
func usualMeetings(items []weekItem, before, from time.Time, span time.Duration) int {
	total := 0
	for k := 0; k < usualWeeks; k++ {
		a := before.Add(time.Duration(k) * span)
		b := a.Add(span)
		if b.After(from) {
			b = from
		}
		total += weekOf(items, a, b, nil, nil).MeetingMinutes
	}
	return total / usualWeeks
}

func minutesOf(in []Interval) int {
	var d time.Duration
	for _, i := range in {
		d += i.End.Sub(i.Start)
	}
	return int(d / time.Minute)
}

// cutAll takes every interval in cuts out of in. Pure.
func cutAll(in, cuts []Interval) []Interval {
	for _, c := range cuts {
		in = CutBusy(in, c)
	}
	return in
}
