package Calendar

// The calendar's kinds of the Access Resolver (PROGRAM.md §5 and CAL5),
// answering with today's rules, each read through the query today's code
// reads it with:
//
//   - an event (its uuid): its creator manages it and its guests see it
//     (GetEvent, UpdateEvent, DeleteEvent); anyone else learns nothing of
//     it, as for no event at all;
//   - a booking page (its id): its owner manages it, and while it is live
//     anyone sees it, signed in or not (GET /public/book/{slug}).
//
// In the graph each id is one read, as the old handlers make it; in the one
// store (A1) every id is decided by one read. A6 gives these the calendar's five levels (free and
// busy, details, edit with private as busy, edit, manage) and registers the
// calendar kind once calendars exist (A1's migration); the kinds stay.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

// The calendar's kinds of resource, as the Access Resolver and Resource Events
// name them. Never renamed once shipped.
const (
	KindEvent   = "event"   // its uuid
	KindBooking = "booking" // a booking page's id
)

func init() {
	accessBusiness.RegisterKind(KindEvent, eventRules{})
	accessBusiness.RegisterKind(KindBooking, bookingRules{})
	// Repeating events are the one store's: the web asks before it offers one,
	// and is told yes once the events are there (not while they move).
	helpers.RegisterFeature(helpers.FeatureNameCalendarRepeats, helpers.CalendarReadsFromStore)
}

// Reasons, as codes the web app words.
const (
	reasonCreator = "creator"
	reasonGuest   = "guest"
	reasonOwner   = "owner"
	reasonPublic  = "public"
)

type eventRules struct{}

func (eventRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	if helpers.CalendarReadsFromStore() {
		return stored.decideMany(ctx, who, ids)
	}
	out := map[string]accessBusiness.Decision{}
	for _, id := range ids {
		eventID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		ev, err := domain.GetDgraphEventInfoByUUID(ctx, eventID.String())
		if errors.Is(err, domain.ErrEventNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = eventDecision(ev, who.UserUUID)
	}
	return out, nil
}

// eventDecision is today's rule for an event as read: its creator manages it,
// its guests see it, and nobody else does. Pure.
func eventDecision(ev *dgraphStruct.DgraphEvent, userUUID string) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case isEventCreator(ev, userUUID):
		d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{reasonCreator}
	case isEventParticipant(ev, userUUID):
		d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonGuest}
	}
	d.Was = d.Level
	return d
}

// decideMany is the event rule over the one store: one read for every id. An
// event in the trash is none for everyone, with Was what they had, and its
// organiser still its owner. An instance of a repeating event is decided as
// its series is.
func (s *service) decideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	byID := map[uuid.UUID][]string{}
	var keys []uuid.UUID
	for _, id := range ids {
		if u, _, err := parseRef(id); err == nil {
			if byID[u] == nil {
				keys = append(keys, u)
			}
			byID[u] = append(byID[u], id)
		}
	}
	rows, err := s.store.getMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	guests, err := s.store.attendees(ctx, idsOf(rows))
	if err != nil {
		return nil, err
	}
	person, _ := uuid.Parse(who.UserUUID)
	for _, e := range rows {
		var d accessBusiness.Decision
		switch roleOf(e, guests[e.ID], person) {
		case roleOrganiser:
			d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{reasonCreator}
		case roleGuest:
			d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonGuest}
		}
		d.Was = d.Level
		if e.DeletedAt != nil {
			d.Deleted, d.Level = true, accessBusiness.LevelNone
		}
		for _, id := range byID[e.ID] {
			out[id] = d
		}
	}
	return out, nil
}

type bookingRules struct{}

func (bookingRules) DecideMany(_ context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	for _, id := range ids {
		pageID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		p, err := bookingModel.PageById(pageID)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out[id] = pageDecision(p, who.UserUUID)
		}
	}
	return out, nil
}

// pageDecision is today's rule for a booking page: its owner manages it, and
// a live one (on, not deleted) is anyone's to see. A deleted page is none for
// everyone, its owner too, with Was saying what they had. Pure.
func pageDecision(p *bookingModel.Page, userUUID string) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case userUUID != "" && p.UserId.String() == userUUID:
		d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{reasonOwner}
	case pageLive(p):
		d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonPublic}
	}
	d.Was = d.Level
	if p.DeletedAt != nil {
		d.Deleted, d.Level = true, accessBusiness.LevelNone
	}
	return d
}

func init() {
	// What an inbox notice, a chip or a link names an event by: its title,
	// for a reader who may open it now (business/Access ReadableTitles). The
	// graph's events are named by nobody here: their notices (a guest taken
	// off, a booking) carry what they say themselves.
	accessBusiness.RegisterTitleReader(KindEvent, func(ctx context.Context, ids []string, reader accessBusiness.Subject) (map[string]string, error) {
		if !helpers.CalendarReadsFromStore() {
			return map[string]string{}, nil
		}
		return stored.eventTitles(ctx, ids, reader)
	})
}

// eventTitles names, of ids, the events reader may open now: each one's
// title, by id as asked. A trashed event is open to nobody.
func (s *service) eventTitles(ctx context.Context, ids []string, reader accessBusiness.Subject) (map[string]string, error) {
	out := map[string]string{}
	decided, err := s.decideMany(ctx, reader, ids)
	if err != nil {
		return nil, err
	}
	byEvent := map[uuid.UUID][]string{}
	var open []uuid.UUID
	for _, id := range ids {
		if d, ok := decided[id]; ok && d.Level >= accessBusiness.LevelView {
			if u, _, err := parseRef(id); err == nil {
				if byEvent[u] == nil {
					open = append(open, u)
				}
				byEvent[u] = append(byEvent[u], id)
			}
		}
	}
	rows, err := s.store.getMany(ctx, open)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if e.DeletedAt != nil {
			continue
		}
		for _, id := range byEvent[e.ID] {
			out[id] = e.Title
		}
	}
	return out, nil
}
