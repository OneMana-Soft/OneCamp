package business

// The engine's read (calendar A3a): who a meeting is for, as the asker may
// see them (calendarBusiness.PeopleSeenBy, Find a time's rule), each one's
// busy times (BusyFor), their own working hours in their zone (Person
// Preferences: calendar.working_hours and zone), and their names for the
// reasons, then Rank. A person who keeps no hours can be asked any time, as
// before; the request's hours, when it sends some, stand for the asker's
// until they keep their own.

import (
	"context"
	"errors"
	"fmt"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// Ranked is what a request for times is answered with: the suggestions, the
// people whose Google calendar couldn't be read (their times may clash
// there), and those whose busy times the workspace keeps from the asker (the
// times don't count them).
type Ranked struct {
	Suggestions []Suggestion `json:"suggestions"`
	Unread      []string     `json:"unread,omitempty"`
	Private     []string     `json:"private,omitempty"`
}

const (
	maxPeople = 20
	maxRange  = 14 * 24 * time.Hour
)

// Seams for tests.
var (
	busyFor      = calendarBusiness.BusyFor
	hoursOf      = calendarBusiness.HoursOf
	peopleSeenBy = calendarBusiness.PeopleSeenBy
	peopleOf     = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		return userDomain.GetPeopleByUUIDs(ctx, ids)
	}
)

// Suggestions answers a request for times between in.From and in.To: the
// asker and everyone it names, each in their own hours and zone, ranked
// with reasons (Rank).
func Suggestions(ctx context.Context, askerUUID string, in calendarBusiness.FindTimeInput, now time.Time) (*Ranked, error) {
	ids := []string{askerUUID}
	seen := map[string]bool{askerUUID: true}
	for _, p := range in.Participants {
		if _, err := uuid.Parse(p); err != nil {
			return nil, refused("One of those people isn't in this workspace.")
		}
		if !seen[p] {
			seen[p] = true
			ids = append(ids, p)
		}
	}
	if len(ids) > maxPeople {
		return nil, refused(fmt.Sprintf("Find a time for up to %d people.", maxPeople))
	}
	if in.DurationMinutes < 15 || in.DurationMinutes > 8*60 {
		return nil, refused("Meetings run from 15 minutes to 8 hours.")
	}
	from, to := in.From, in.To
	if from.Before(now) {
		from = now
	}
	if !to.After(from) {
		return nil, refused("Pick a range that ends after it starts.")
	}
	if to.Sub(from) > maxRange {
		to = from.Add(maxRange)
	}

	// Only those the asker may see when they're busy count (the calendar's
	// one rule, as for Find a time): the rest are named private, unread.
	ids, private, err := peopleSeenBy(ctx, askerUUID, ids)
	if err != nil {
		return nil, errors.New("couldn't read calendars")
	}
	uuids := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		uuids[i] = uuid.MustParse(id)
	}
	hours, err := hoursOf(ctx, uuids)
	if err != nil {
		return nil, errors.New("couldn't read working hours")
	}
	names := map[string]string{}
	if found, err := peopleOf(ctx, ids); err == nil {
		for _, u := range found {
			if u != nil {
				names[u.Uuid] = u.DisplayName()
			}
		}
	}

	// Everyone's busy times at once, eight reads at a time (twenty people in
	// one wait, not twenty).
	busy := make([]calendarBusiness.Busy, len(ids))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, id := range ids {
		g.Go(func() error {
			b, err := busyFor(gctx, id, from, to)
			busy[i] = b
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, errors.New("couldn't read calendars")
	}
	res := &Ranked{Private: private}
	people := make([]Person, len(ids))
	zone := time.UTC
	for i, id := range ids {
		b := busy[i]
		if b.GoogleUnread {
			res.Unread = append(res.Unread, id)
		}
		p := Person{ID: id, Name: names[id], Busy: b.Intervals}
		if p.Name == "" {
			p.Name = "Someone"
		}
		if h, ok := hours[uuids[i]]; ok {
			h := h
			p.Hours = &h
		} else if i == 0 {
			// The asker's hours as the request sends them, until they keep their own.
			if _, err := in.Hours.Check(); err == nil {
				h := in.Hours
				p.Hours = &h
			}
		}
		if i == 0 && p.Hours != nil {
			if loc, err := time.LoadLocation(p.Hours.TZ); err == nil {
				zone = loc
			}
		}
		people[i] = p
	}
	res.Suggestions = Rank(people, Ask{
		Duration: time.Duration(in.DurationMinutes) * time.Minute,
		Step:     15 * time.Minute,
		From:     from,
		To:       to,
		// From the quarter hour after now, so the first isn't 10:07.
		Earliest: now.Truncate(15 * time.Minute).Add(15 * time.Minute),
		Zone:     zone,
		PerDay:   3,
		Limit:    12,
	})
	if res.Suggestions == nil {
		res.Suggestions = []Suggestion{}
	}
	return res, nil
}

// refused is a request answered with words for the person.
func refused(msg string) error { return &Refusal{msg} }

// Refusal is a request for times that can't be answered as asked.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }
