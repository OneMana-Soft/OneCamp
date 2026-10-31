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
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
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
	// reasonCalendar is an event on a calendar the reader may see.
	reasonCalendar = "calendar"
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
	// An event on a calendar someone may see (a project's, A6) is theirs to see.
	var calendars []string
	for _, e := range rows {
		if roleOf(e, guests[e.ID], person) == roleNone {
			calendars = append(calendars, e.CalendarID.String())
		}
	}
	onCalendar := map[string]accessBusiness.Decision{}
	if len(calendars) > 0 {
		if onCalendar, err = s.decideCalendars(ctx, who, calendars); err != nil {
			return nil, err
		}
	}
	for _, e := range rows {
		var d accessBusiness.Decision
		switch roleOf(e, guests[e.ID], person) {
		case roleOrganiser:
			d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{reasonCreator}
		case roleGuest:
			d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonGuest}
		case roleReader:
			d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonSubscribed}
		default:
			// What shows as Busy to them is theirs to open nowhere else.
			if c := onCalendar[e.CalendarID.String()]; c.Level >= accessBusiness.LevelView && !hiddenFrom(e, guests[e.ID], person, c) {
				d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonCalendar}
			}
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
	// How a chip, an inbox notice, a link and the work graph name an event,
	// for a reader who may open it now; the kind's title reader too
	// (business/Access ReadableTitles). The graph's old events are named by
	// nobody here: their notices (a guest taken off, a booking) carry what
	// they say themselves.
	entityLinkBusiness.RegisterKind(KindEvent, func(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]entityLinkBusiness.Entity, error) {
		if !helpers.CalendarReadsFromStore() {
			return map[string]entityLinkBusiness.Entity{}, nil
		}
		return stored.eventChips(ctx, who, ids)
	})
}

// eventChips is each of ids, an event or one instance of a series, as who
// may see it now, by id as asked: its title and when it starts while they
// may open it, and gone once it is in the trash for someone who could. One
// decision (decideMany) and one read of the events.
func (s *service) eventChips(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]entityLinkBusiness.Entity, error) {
	decided, err := s.decideMany(ctx, who, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]entityLinkBusiness.Entity{}
	byEvent := map[uuid.UUID][]string{}
	var open []uuid.UUID
	for _, id := range ids {
		d, ok := decided[id]
		switch {
		case !ok:
		case d.Deleted && d.Was >= accessBusiness.LevelView:
			out[id] = entityLinkBusiness.Entity{Kind: KindEvent, ID: id, Access: entityLinkBusiness.AccessGone}
		case d.Level >= accessBusiness.LevelView:
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
	title := s.titler(ctx, who, rows)
	for _, e := range rows {
		if e.DeletedAt != nil {
			continue
		}
		for _, id := range byEvent[e.ID] {
			_, rid, _ := parseRef(id)
			out[id] = entityLinkBusiness.Entity{Kind: KindEvent, ID: id, Access: entityLinkBusiness.AccessOK, Title: title(e),
				Data: map[string]any{"title": title(e), "start": refStart(e, rid), "all_day": e.AllDay(), "repeats": e.RRule != ""}}
		}
	}
	return out, nil
}

// refStart is when what a ref names starts: the event, or the instance of
// its series at rid (its start as the series gave it), in the event's zone.
// Pure.
func refStart(e *models.Event, rid *time.Time) When {
	start, _ := whenOf(e)
	switch {
	case rid == nil:
		return start
	case e.AllDay():
		return When{Date: rid.Format(dateLayout), Zone: e.TZ}
	}
	return When{At: rid, Zone: e.TZ}
}
