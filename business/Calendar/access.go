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
// Each id is one read, as today's handlers make it; nothing decides many at
// once before A1's tables. A6 gives these the calendar's five levels (free and
// busy, details, edit with private as busy, edit, manage) and registers the
// calendar kind once calendars exist (A1's migration); the kinds stay.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
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
