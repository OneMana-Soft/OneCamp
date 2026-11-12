package Calendar

// Every event's call room (gcal-15 head B, "Give every event a call room on
// Call State"). Rule 1, who has one: every OneCamp event, one-off or series
// (one room per series, as calls are), without anyone adding it. Nothing is
// made when the event is: no row, no LiveKit room, no email. Its room is made
// ready the first time someone joins it (business/CallRoom ReadyEventRoom: a
// meeting whose id is the event's, so Call State files what the call makes
// onto the event by its one mapping, HomeOf).
//
// Left out, and why:
//   - a copy a calendar file or a provider's sync brought (sourceICS: imports,
//     Google and Microsoft 365 sync-in) and one a calendar read from an address
//     keeps in step (sourceSubscribed) carry their own link, where the call is;
//   - a booking hold has nobody to call: it holds time for a booking page;
//   - own time (focus, away, a working location, a task block, a reminder):
//     nobody else is in it.
//
// An event given a guest link keeps that room whatever it is (events.call_room).
//
// ONE room per event: an event that holds a call (call_room, the guest link's
// room) is joined in that call; else in its own room, "meet-<its id>". A guest
// link added later goes on the room members are in (business/CallRoom
// EventCall adopts it), so a call going on is never split.
//
// Permission model: who joins is the calendar's own event decision
// (decideMany, Level >= View): its organiser, its guests, a reader of its
// calendar above busy-only, and a calendar editor who sees a private event
// (as Google lets them). A busy-only reader, someone not on it, and a reader
// of a private event who may not change its calendar are told there is no
// such event. The routes are a session's (no API token joins a call); the
// same rule is asked again when someone connects with their token
// (mayJoinCallIn, through business/LiveKit's MemberMayBeInCall), so a token
// outlives no right. Who may add or take off the guest link is unchanged
// (mayChange, eventCall.go).

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ownRoomPrefix is an event's own room's ("meet-<its id>"), the prefix every
// meeting's room has (business/Guest MeetingRoomPrefix).
const ownRoomPrefix = "meet-"

// RoomJoinPath is the app's page a member joins an event's room at, before
// the event's id ("/app/meet/e/<id>").
const RoomJoinPath = "/app/meet/e/"

// guestLinksOn is whether a guest link may be added to an event's room (a seam
// for tests): guest access and its rooms on.
var guestLinksOn = settingsBusiness.GuestRoomsEnabled

// hasOwnRoom is rule 1: a OneCamp event, of the event kind. Pure.
func hasOwnRoom(e *models.Event) bool {
	return (e.Source == sourceOneCamp || e.Source == sourceMoved) && EventKind(e.Kind) == EventPlain
}

// roomOf is the event's call room: the call it holds, else its own room by
// rule 1, else "" for none. An instance's is its series' (call it on the
// owner). Pure.
func roomOf(e *models.Event) string {
	switch {
	case e.CallRoom != "":
		return e.CallRoom
	case hasOwnRoom(e):
		return ownRoomPrefix + e.ID.String()
	}
	return ""
}

// roomView is what an event's read says of its room, joined at ref's page,
// with whether a guest link may be added (guestLinksOn, read once per read);
// nil for an event without one, or in the trash. Pure.
func roomView(e *models.Event, ref string, guestLinks bool) *dgraphStruct.EventRoom {
	if e.DeletedAt != nil || roomOf(e) == "" {
		return nil
	}
	return &dgraphStruct.EventRoom{Join: RoomJoinPath + ref, GuestLinks: guestLinks}
}

// Joining is an event's room for someone who may join it.
type Joining struct {
	Room  string
	Title string
}

// JoinRoom is the room of the event id names (a series' for any of its
// instances) for whoever may read its details, made ready for its first join
// when ready (nothing is made by a read of where to join). Anyone else is
// told there is no such event; an event without a room, ErrNoEventCall.
func (s *service) JoinRoom(ctx context.Context, who *model.UserInfo, id string, ready bool) (*Joining, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	key, _, err := parseRef(id)
	if err != nil {
		return nil, err
	}
	owner, _, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if owner.SeriesID != nil {
		// An instance changed on its own: its series' room.
		if owner, _, err = s.load(ctx, *owner.SeriesID); err != nil {
			return nil, err
		}
	}
	decided, err := s.decideMany(ctx, subjectOf(who), []string{owner.ID.String()})
	if err != nil {
		return nil, err
	}
	if decided[owner.ID.String()].Level < accessBusiness.LevelView {
		return nil, ErrEventNotFound
	}
	room := roomOf(owner)
	if room == "" {
		return nil, ErrNoEventCall
	}
	if ready {
		if eventCalls.Ready == nil {
			return nil, ErrNoEventCalls
		}
		if err := eventCalls.Ready(ctx, owner.ID, owner.CreatedBy, owner.Title, room); err != nil {
			return nil, err
		}
	}
	return &Joining{Room: room, Title: owner.Title}, nil
}

// MayJoinCallIn reports whether who may be in the call in room now, the call
// of event (its meeting's: business/CallState HomeOf), by JoinRoom's rule:
// they may read the details of an event whose room it is. A series and a part
// of it split off share a call that knows only the series, so the events
// holding the room are asked too. business/CallState asks it for
// business/LiveKit when someone connects, and for a read of the call.
func MayJoinCallIn(ctx context.Context, who *model.UserInfo, room string, event uuid.UUID) (bool, error) {
	return stored.mayJoinCallIn(ctx, who, room, event)
}

func (s *service) mayJoinCallIn(ctx context.Context, who *model.UserInfo, room string, event uuid.UUID) (bool, error) {
	if room == "" {
		return false, nil
	}
	if event != uuid.Nil {
		rows, err := s.store.getMany(ctx, []uuid.UUID{event})
		if err != nil {
			return false, err
		}
		if ok, err := s.mayJoinAny(ctx, who, room, rows); err != nil || ok {
			return ok, err
		}
	}
	rows, err := s.store.holdingRoom(ctx, room)
	if err != nil {
		return false, err
	}
	return s.mayJoinAny(ctx, who, room, rows)
}

// mayJoinAny is whether who may read the details of one of rows whose room is
// room, live, in one decision.
func (s *service) mayJoinAny(ctx context.Context, who *model.UserInfo, room string, rows []*models.Event) (bool, error) {
	var ids []string
	for _, e := range rows {
		if e.DeletedAt == nil && roomOf(e) == room {
			ids = append(ids, e.ID.String())
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	decided, err := s.decideMany(ctx, subjectOf(who), ids)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if decided[id].Level >= accessBusiness.LevelView {
			return true, nil
		}
	}
	return false, nil
}
