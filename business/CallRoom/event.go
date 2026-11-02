package business

// An event's own call (calendar B11; calls zoom-01, meet-02, zoom-15), on the
// personal room's machinery, keyed by its meeting rather than its owner: a
// meeting of kind event, the event's organiser's and named for it, with a
// lobby like any meeting's, and one standing link at an address of the event's
// title and a few random letters ("/r/weekly-sync-3f9a1c2e").
//
// Who comes in is every meeting's rule: a member at its address comes in as
// themselves and never waits (D-CL-5, business/CallInvite); the event's
// invitees come in at once by their own invitation (meet-02, the lobby's
// 'invitees'); anyone else from outside knocks and waits for whoever runs the
// call; the organiser, its meeting's owner, runs it. business/Calendar makes one when someone who may change the
// event adds a call, and closes it when the call is taken off, or the event is
// deleted.

import (
	"context"
	"errors"
	"strings"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	meetingOf    = model.MeetingOf
	newEventRoom = guestBusiness.NewEventRoom
	addressesOf  = guestModel.AddressesOf
	lobbiesOf    = model.LobbiesOf
)

// maxTitleRunes is the most of an event's title its meeting keeps
// (call_meetings.title is varchar(200)).
const maxTitleRunes = 200

// EventCall makes a call for an event its organiser runs: the meeting, which
// knows its event (where what the call makes lands: business/CallState
// HomeOf), and its link at an address, in one transaction, under the switches
// a personal room is under. Two calls of one title at once get different
// addresses; one whose random letters are taken tries once more.
func EventCall(ctx context.Context, event, organiser uuid.UUID, eventTitle string) (*Room, error) {
	if !guestOn() {
		return nil, guestBusiness.ErrGuestDisabled
	}
	if !roomsAllowed() {
		return nil, guestBusiness.ErrRoomsOff
	}
	m, err := eventCall(ctx, event, organiser, eventTitle)
	if errors.Is(err, guestBusiness.ErrSlugTaken) {
		m, err = eventCall(ctx, event, organiser, eventTitle)
	}
	if err != nil {
		return nil, err
	}
	return describe(ctx, m)
}

// eventCall is one try at EventCall, in one transaction.
func eventCall(ctx context.Context, event, organiser uuid.UUID, eventTitle string) (*model.Meeting, error) {
	title := strings.TrimSpace(eventTitle)
	if r := []rune(title); len(r) > maxTitleRunes {
		title = string(r[:maxTitleRunes])
	}
	m := &model.Meeting{ID: uuid.NewString(), Kind: model.KindEvent, Owner: organiser, Title: title, Lobby: model.LobbyInvitees, CreatedAt: now(), Event: event}
	err := inTx(ctx, func(ctx context.Context) error {
		if err := saveMeeting(ctx, *m); err != nil {
			return err
		}
		_, err := newEventRoom(ctx, roomOf(m), organiser, title)
		return err
	})
	return m, err
}

// CloseEventCall ends the event call held in room: its address stops
// answering and its guests are put out, as when a personal room closes. A
// room that is no event call's, or already closed, is left as it is.
func CloseEventCall(ctx context.Context, room string) error {
	id, ok := strings.CutPrefix(room, guestBusiness.MeetingRoomPrefix)
	if !ok {
		return nil
	}
	m, err := meetingOf(ctx, id)
	if err != nil || m == nil || m.Kind != model.KindEvent || m.ArchivedAt != nil {
		return err
	}
	return closeMeeting(ctx, m)
}

// EventCalls is each event call by its room, as the event's readers see it,
// for those whose address answers now: none at all while guest access or its
// rooms are off, so no event offers a link that leads nowhere. One read of
// their addresses and one of their lobbies for a page of events.
func EventCalls(ctx context.Context, rooms []string) (map[string]*dgraphStruct.EventCall, error) {
	out := map[string]*dgraphStruct.EventCall{}
	if len(rooms) == 0 || !roomsOn() {
		return out, nil
	}
	at, err := addressesOf(ctx, rooms)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(at))
	for room := range at {
		if id, ok := strings.CutPrefix(room, guestBusiness.MeetingRoomPrefix); ok {
			ids = append(ids, id)
		}
	}
	lobbies, err := lobbiesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	for room, slug := range at {
		id, _ := strings.CutPrefix(room, guestBusiness.MeetingRoomPrefix)
		out[room] = callOf(room, slug, lobbies[id] == model.LobbyGuests)
	}
	return out, nil
}

// callOf is the call in room as an event's readers see it, at slug, saying
// whether its invitees knock. Pure.
func callOf(room, slug string, inviteesWait bool) *dgraphStruct.EventCall {
	return &dgraphStruct.EventCall{Room: room, Slug: slug, Address: guestBusiness.KindRoom.WebAddress(slug), InviteesWait: inviteesWait}
}

// The calendar makes, ends and finds its events' calls with these: it sits
// below this package, so it is handed them here.
func init() {
	calendarBusiness.OnEventCalls(calendarBusiness.EventCalls{
		Make: func(ctx context.Context, event, organiser uuid.UUID, title string) (*dgraphStruct.EventCall, error) {
			r, err := EventCall(ctx, event, organiser, title)
			if err != nil {
				return nil, err
			}
			return callOf(r.Room, r.Slug, false), nil
		},
		End:   CloseEventCall,
		Calls: EventCalls,
		Lobby: callLobbyBusiness.SetInviteesWait,
	})
}
