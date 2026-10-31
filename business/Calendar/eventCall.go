package Calendar

// An event's own call (calendar B11; calls zoom-01, meet-02, zoom-15). It is
// made on demand by whoever may change the event (its organiser, or a guest
// the organiser lets change it: mayChange) and is held in the event's
// call_room. A series has one call for every instance; an instance asks for
// its series'. Taking it off, or deleting the event, ends it.
//
// The call is a meeting on business/CallRoom's machinery: the organiser runs
// it, members come in as themselves at its address and never wait (D-CL-5),
// and anyone from outside knocks and waits to be let in. Its readers see its
// address (withCalls) wherever they see the event's notes. A guest who may
// not change the event can join its call but neither add one nor take it off.
//
// IMPORTS. business/CallRoom sits above this package (it reaches Project,
// which imports the calendar), so it hands the calendar its functions at
// start (OnEventCalls); until then, no event can be given a call.

import (
	"context"
	"errors"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// EventCalls is how the calendar makes, ends and finds an event's call.
type EventCalls struct {
	// Make makes a call its organiser runs, named for the event: its room
	// and where its guests come in.
	Make func(ctx context.Context, organiser uuid.UUID, title string) (*dgraphStruct.EventCall, error)
	// End ends the call in a room; a room that is no event's call, or ended
	// already, is left as it is.
	End func(ctx context.Context, room string) error
	// Calls is each room's call as its readers see it, for those whose
	// address answers now, in one read.
	Calls func(ctx context.Context, rooms []string) (map[string]*dgraphStruct.EventCall, error)
}

var eventCalls EventCalls

// OnEventCalls hands the calendar the event call functions (business/CallRoom,
// at start).
func OnEventCalls(c EventCalls) { eventCalls = c }

// ErrNoEventCalls is a server whose calls aren't wired: an event can't be
// given a call there.
var ErrNoEventCalls = errors.New("Calls aren't available on this server.")

// AddCall gives an event its own call, or answers the one it has while its
// address answers. Someone who may not change the event is refused as for any
// change. Two at once make one: the second finds the first's, and its own is
// ended.
func (s *service) AddCall(ctx context.Context, who *model.UserInfo, id string) (*dgraphStruct.EventCall, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	e := inst.owner()
	if err := mayChange(roleOf(e, guests, personOf(who)), e); err != nil {
		return nil, err
	}
	if eventCalls.Make == nil {
		return nil, ErrNoEventCalls
	}
	if call := s.liveCall(ctx, e.CallRoom); call != nil {
		return call, nil
	}
	call, err := eventCalls.Make(ctx, e.CreatedBy, e.Title)
	if err != nil {
		return nil, err
	}
	took, err := s.store.setCallRoom(ctx, e.ID, call.Room, e.CallRoom, s.now())
	if err != nil || !took {
		// Nothing holds this call: end it, and answer the one that won.
		s.endCall(ctx, call.Room)
		if err != nil {
			return nil, err
		}
		if cur, err := s.store.get(ctx, e.ID); err == nil && cur != nil {
			if won := s.liveCall(ctx, cur.CallRoom); won != nil {
				return won, nil
			}
		}
		return nil, ErrEventNotFound
	}
	if was := e.CallRoom; was != "" {
		s.endCall(ctx, was)
	}
	e.CallRoom = call.Room
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests), map[string]string{"changed": "call"})
	s.tellCallChanged(ctx, who, e, guests)
	return call, nil
}

// RemoveCall takes an event's call off and ends it, for whoever may change the
// event. An event without one is left as it is.
func (s *service) RemoveCall(ctx context.Context, who *model.UserInfo, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return err
	}
	e := inst.owner()
	if err := mayChange(roleOf(e, guests, personOf(who)), e); err != nil {
		return err
	}
	if e.CallRoom == "" {
		return nil
	}
	took, err := s.store.setCallRoom(ctx, e.ID, "", e.CallRoom, s.now())
	if err != nil || !took {
		return err
	}
	s.endCall(ctx, e.CallRoom)
	e.CallRoom = ""
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests), map[string]string{"changed": "call"})
	s.tellCallChanged(ctx, who, e, guests)
	return nil
}

// tellCallChanged sends e's guests by email its updated invitation when its
// call is added or taken off (meet-02): the calendar file with the call's link
// or without it, and the email's Join call with their own key. Members hear of
// it live, by the announce. "Don't send" sends nothing, as for any change, and
// an event that is over mails nobody.
func (s *service) tellCallChanged(ctx context.Context, who *model.UserInfo, e *models.Event, guests []models.Attendee) {
	if len(emailGuests(guests)) == 0 {
		return
	}
	if last := lastEnd(e); last != nil && s.now().After(*last) {
		return // over: nobody joins it now, so nobody is mailed
	}
	s.tells.tell(ctx, who, telling{what: toldChanged, event: e, was: e, now: e, guestsBefore: guests, guestsAfter: guests,
		changed: []string{"call"}, outsidersOnly: true})
}

// liveCall is the call in room while its address answers, nil otherwise.
func (s *service) liveCall(ctx context.Context, room string) *dgraphStruct.EventCall {
	if room == "" || eventCalls.Calls == nil {
		return nil
	}
	calls, err := eventCalls.Calls(ctx, []string{room})
	if err != nil {
		return nil
	}
	return calls[room]
}

// endCall ends the call in room, saying so in the log when it can't: the
// event no longer holds it either way.
func (s *service) endCall(ctx context.Context, room string) {
	if room == "" || eventCalls.End == nil {
		return
	}
	if err := eventCalls.End(ctx, room); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the call in %s wasn't ended: %v", room, err)
	}
}

// withCalls gives each event read its call, in one read for them
// all, and takes off a call whose address doesn't answer now (guest access or
// its rooms off, the link turned off): no event offers a link that leads
// nowhere.
func (s *service) withCalls(ctx context.Context, out []*dgraphStruct.DgraphEvent) {
	var rooms []string
	for _, ev := range out {
		if ev.Call != nil {
			rooms = append(rooms, ev.Call.Room)
		}
	}
	if len(rooms) == 0 {
		return
	}
	calls := map[string]*dgraphStruct.EventCall{}
	if eventCalls.Calls != nil {
		var err error
		if calls, err = eventCalls.Calls(ctx, rooms); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: events' calls weren't read: %v", err)
		}
	}
	for _, ev := range out {
		if ev.Call != nil {
			if c := calls[ev.Call.Room]; c != nil {
				ev.Call = &dgraphStruct.EventCall{Room: c.Room, Slug: c.Slug, Address: c.Address}
			} else {
				ev.Call = nil
			}
		}
	}
}
