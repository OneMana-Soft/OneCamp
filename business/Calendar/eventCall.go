package Calendar

// An event's own call (calendar B11; calls zoom-01, meet-02, zoom-15). It is
// made on demand by whoever may change the event (its organiser, or a guest
// the organiser lets change it: mayChange) and is held in the event's
// call_room. A series has one call for every instance; an instance asks for
// its series'. Taking it off ends it. Deleting the event closes it and keeps
// it in the event's call_room, and bringing the event back from the trash
// opens it again at the same address (V-641), as Google's restored event
// keeps its Meet link; one taken off before the delete stays off.
//
// The call is the event's it was made for. "This and following" gives the new
// series its series' call to share, but only the series ends it or opens it
// again: a new series deleted, or its call taken off, leaves the series' call
// answering, and one back from the trash comes back without it (main, 6 Nov),
// offering to add its own, as does an instance back on its own.
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
	// Make gives the event (its id) the guest link of its call that its
	// organiser runs, named for it: its room and where its guests come in. It
	// goes on room, the event's room (roomOf), when that room's call is the
	// event's own, so a call members are in is never split (gcal-15 head B);
	// else on the event's own room.
	Make func(ctx context.Context, event, organiser uuid.UUID, title, room string) (*dgraphStruct.EventCall, error)
	// Ready makes the event's own room ready for its first join (gcal-15 head
	// B): its meeting, the event's, with no link, made once. A room that is a
	// call made before is ready as it is.
	Ready func(ctx context.Context, event, organiser uuid.UUID, title, room string) error
	// End ends the call in a room that was made for the event (its id); a
	// room that is another event's call, or no event's, or ended already, is
	// left as it is.
	End func(ctx context.Context, event uuid.UUID, room string) error
	// Calls is each room's call as its readers see it, for those whose
	// address answers now, in one read.
	Calls func(ctx context.Context, rooms []string) (map[string]*dgraphStruct.EventCall, error)
	// Lobby makes the invitees of the event call in room knock (true) or come
	// in by their own invitation again, answering whether it is an open one.
	Lobby func(ctx context.Context, room string, inviteesWait bool) (bool, error)
	// Reopen opens again the call in a room that was made for the event (its
	// id) and ended with it, at the address it had, as the event comes back
	// from the trash, answering whether the room is the event's call. A room
	// that is open is left as it is; one that is another event's call, or no
	// event's, is left as it is and answered false.
	Reopen func(ctx context.Context, event uuid.UUID, room string) (bool, error)
}

var eventCalls EventCalls

// OnEventCalls hands the calendar the event call functions (business/CallRoom,
// at start).
func OnEventCalls(c EventCalls) { eventCalls = c }

// ErrNoEventCalls is a server whose calls aren't wired: an event can't be
// given a call there.
var ErrNoEventCalls = errors.New("Calls aren't available on this server.")

// ErrNoEventCall is an event without a call of its own to change.
var ErrNoEventCall = errors.New("This event has no call.")

// AddCall gives an event its own call, or answers the one it has while its
// address answers: the guest link of the room its members are in (gcal-15
// head B), the same room when a link stopped answering. Someone who may not
// change the event is refused as for any change. Two at once make one: the
// second finds the first's, and its own is ended unless it is that room.
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
	call, err := eventCalls.Make(ctx, e.ID, e.CreatedBy, e.Title, roomOf(e))
	if err != nil {
		return nil, err
	}
	took, err := s.store.setCallRoom(ctx, e.ID, call.Room, e.CallRoom, s.now())
	if err != nil || !took {
		// Answer the call that won; end this one unless it is the same room.
		cur, gerr := s.store.get(ctx, e.ID)
		if gerr != nil || cur == nil || cur.CallRoom != call.Room {
			s.endCall(ctx, e.ID, call.Room)
		}
		if err != nil {
			return nil, err
		}
		if cur != nil {
			if won := s.liveCall(ctx, cur.CallRoom); won != nil {
				return won, nil
			}
		}
		return nil, ErrEventNotFound
	}
	if was := e.CallRoom; was != "" && was != call.Room {
		s.endCall(ctx, e.ID, was)
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
	s.endCall(ctx, e.ID, e.CallRoom)
	e.CallRoom = ""
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests), map[string]string{"changed": "call"})
	s.tellCallChanged(ctx, who, e, guests)
	return nil
}

// SetCallLobby makes the event's invitees knock at its call like anyone from
// outside, or come in by their own invitation again (meet-02, Meet's quick
// access off), for whoever may change the event, and answers its call. Its
// guests by email aren't mailed: their invitation and its link are the same.
func (s *service) SetCallLobby(ctx context.Context, who *model.UserInfo, id string, inviteesWait bool) (*dgraphStruct.EventCall, error) {
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
	if eventCalls.Lobby == nil {
		return nil, ErrNoEventCalls
	}
	if s.liveCall(ctx, e.CallRoom) == nil {
		return nil, ErrNoEventCall
	}
	found, err := eventCalls.Lobby(ctx, e.CallRoom, inviteesWait)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNoEventCall
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests), map[string]string{"changed": "call"})
	return s.liveCall(ctx, e.CallRoom), nil
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

// endCall ends the call in room made for the event (its id), saying so in
// the log when it can't: the event no longer holds it either way. A call it
// shares but wasn't made for (a new series' of "this and following") answers
// on, for the event it was made for.
func (s *service) endCall(ctx context.Context, event uuid.UUID, room string) {
	if room == "" || eventCalls.End == nil {
		return
	}
	if err := eventCalls.End(ctx, event, room); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the call in %s wasn't ended: %v", room, err)
	}
}

// callBack opens again the call that closed with e, as e comes back from the
// trash, at its address (V-641). A call e held but wasn't made for (its
// series' call, for a new series of "this and following" or an instance back
// on its own) is never reached: e comes back without it, offering to add its
// own, so nobody is pulled back into a call a deleted part of a series shared
// (main, 6 Nov). It says so in the log when it can't: e is back either way.
func (s *service) callBack(ctx context.Context, e *models.Event) {
	if e.CallRoom == "" || eventCalls.Reopen == nil {
		return
	}
	mine, err := eventCalls.Reopen(ctx, e.ID, e.CallRoom)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the call in %s wasn't opened again: %v", e.CallRoom, err)
		return
	}
	if mine {
		return
	}
	if _, err := s.store.setCallRoom(ctx, e.ID, "", e.CallRoom, s.now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the call in %s wasn't taken off %s: %v", e.CallRoom, e.ID, err)
		return
	}
	e.CallRoom = ""
}

// withCalls gives each event read its call, in one read for them
// all, and takes off a call whose address doesn't answer now (guest access or
// its rooms off, the link turned off): no event offers a link that leads
// nowhere. Each room says whether a guest link may be added to it now
// (gcal-15 head B), by one read of the switch.
func (s *service) withCalls(ctx context.Context, out []*dgraphStruct.DgraphEvent) {
	guestLinks := guestLinksOn()
	var rooms []string
	for _, ev := range out {
		if ev.Room != nil {
			ev.Room.GuestLinks = guestLinks
		}
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
				ev.Call = &dgraphStruct.EventCall{Room: c.Room, Slug: c.Slug, Address: c.Address, InviteesWait: c.InviteesWait}
			} else {
				ev.Call = nil
			}
		}
	}
}
