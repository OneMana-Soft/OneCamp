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
// deleted; an event back from the trash opens its call again at the same
// address (calendar V-641). Only the event a call was made for (its meeting's
// Event) ends it or opens it again: a new series of "this and following"
// shares its series' call but never reaches it (main, 6 Nov).
//
// Every OneCamp event has a room (gcal-15 head B, business/Calendar
// eventRoom.go): its own is "meet-<its id>", a meeting whose id IS the
// event's, made ready on the first join (ReadyEventRoom) with no link and
// under no guest switch: members need no guest access. A guest link goes on
// the room members are in (EventCall adopts the event's room), so a call is
// never split; only the link is under the guest switches.

import (
	"context"
	"errors"
	"strings"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	meetingOf    = model.MeetingOf
	lockMeeting  = model.MeetingForUpdate
	newEventRoom = guestBusiness.NewEventRoom
	addressesOf  = guestModel.AddressesOf
	lobbiesOf    = model.LobbiesOf
	hostByRule   = liveKitBusiness.HostByRule
	mintToken    = liveKitBusiness.CreateRoomAndGetToken
)

// maxTitleRunes is the most of an event's title its meeting keeps
// (call_meetings.title is varchar(200)).
const maxTitleRunes = 200

// EventCall gives an event the guest link of its call that its organiser
// runs, under the switches a personal room is under: on room, the event's
// room, when that room's meeting is the event's own call (its own room, or a
// call made for it before), else on the event's own room; never on a call
// another event's was made for (a series' a new series of "this and
// following" shares). The meeting knows its event (where what the call makes
// lands: business/CallState HomeOf); one closed opens again, and a link is
// made only when none answers, all in one transaction under the meeting's
// row, so two at once make one link. Two calls of one title at once get
// different addresses; one whose random letters are taken tries once more.
func EventCall(ctx context.Context, event, organiser uuid.UUID, eventTitle, room string) (*Room, error) {
	if !guestOn() {
		return nil, guestBusiness.ErrGuestDisabled
	}
	if !roomsAllowed() {
		return nil, guestBusiness.ErrRoomsOff
	}
	m, err := eventCall(ctx, event, organiser, eventTitle, room)
	if errors.Is(err, guestBusiness.ErrSlugTaken) {
		m, err = eventCall(ctx, event, organiser, eventTitle, room)
	}
	if err != nil {
		return nil, err
	}
	return describe(ctx, m)
}

// eventCall is one try at EventCall, in one transaction.
func eventCall(ctx context.Context, event, organiser uuid.UUID, eventTitle, room string) (*model.Meeting, error) {
	id := event.String()
	made, err := eventMeeting(ctx, event, room)
	if err != nil {
		return nil, err
	}
	if made != nil {
		id = made.ID
	}
	m := eventRoomMeeting(id, event, organiser, eventTitle)
	err = inTx(ctx, func(ctx context.Context) error {
		if err := saveMeeting(ctx, *m); err != nil {
			return err
		}
		cur, err := lockMeeting(ctx, id)
		if err != nil {
			return err
		}
		if cur == nil {
			return errors.New("callroom: the event's meeting wasn't kept")
		}
		*m = *cur
		if m.ArchivedAt != nil {
			if _, err := reopenMeeting(ctx, m.ID, *m.ArchivedAt); err != nil {
				return err
			}
			m.ArchivedAt = nil
		}
		links, err := liveLinks(ctx, guestModel.ResourceMeeting, roomOf(m))
		if err != nil || addressOf(links) != nil {
			return err
		}
		_, err = newEventRoom(ctx, roomOf(m), organiser, m.Title)
		return err
	})
	return m, err
}

// eventRoomMeeting is a new meeting of an event's call, id, the organiser's
// and named for the event, whose invitees come in by their own invitation.
func eventRoomMeeting(id string, event, organiser uuid.UUID, eventTitle string) *model.Meeting {
	title := strings.TrimSpace(eventTitle)
	if r := []rune(title); len(r) > maxTitleRunes {
		title = string(r[:maxTitleRunes])
	}
	return &model.Meeting{ID: id, Kind: model.KindEvent, Owner: organiser, Title: title, Lobby: model.LobbyInvitees, CreatedAt: now(), Event: event}
}

// ReadyEventRoom makes an event's own room (room, "meet-<its id>") ready for
// its first join: its meeting, the event's, with no link, under no guest
// switch, kept as it is when it is there. A room that is a call made before
// is ready as it is (gcal-15 head B).
func ReadyEventRoom(ctx context.Context, event, organiser uuid.UUID, eventTitle, room string) error {
	if room != guestBusiness.MeetingRoomPrefix+event.String() {
		return nil
	}
	return saveMeeting(ctx, *eventRoomMeeting(event.String(), event, organiser, eventTitle))
}

// EnterEventRoom mints a member's token for an event's room, which the
// calendar has let them join (business/Calendar JoinRoom): its organiser runs
// it (its meeting's owner, HostByRule); a lock or a host's removal still keeps
// anyone out, in their own words (the mint's MayEnter).
func EnterEventRoom(ctx context.Context, room string, user *dgraphStruct.DgraphUser, language string) (string, error) {
	host, err := hostByRule(ctx, room, user, false)
	if err != nil {
		return "", err
	}
	token, _, err := mintToken(ctx, room, user, host, language)
	return token, err
}

// CloseEventCall ends the call held in room that was made for event: its
// address stops answering and its guests are put out, as when a personal room
// closes. A room that is another event's call, or no event call's, or already
// closed, is left as it is.
func CloseEventCall(ctx context.Context, event uuid.UUID, room string) error {
	m, err := eventMeeting(ctx, event, room)
	if err != nil || m == nil || m.ArchivedAt != nil {
		return err
	}
	return closeMeeting(ctx, m)
}

// eventMeeting is the meeting of the call held in room that was made for
// event, nil for a room that is another event's call or no event call's (one
// whose event was purged knows none).
func eventMeeting(ctx context.Context, event uuid.UUID, room string) (*model.Meeting, error) {
	id, ok := strings.CutPrefix(room, guestBusiness.MeetingRoomPrefix)
	if !ok || event == uuid.Nil {
		return nil, nil
	}
	m, err := meetingOf(ctx, id)
	if err != nil || m == nil || m.Kind != model.KindEvent || m.Event != event {
		return nil, err
	}
	return m, nil
}

// errStaysClosed rolls back a reopen whose address doesn't answer again.
var errStaysClosed = errors.New("callroom: the event call stays closed")

// ReopenEventCall opens again the call held in room that was made for event,
// as closing left it (calendar V-641: the event is back from the trash): its
// meeting, and the links that ended with it, at the address they had, in one
// transaction, so the link its guests already have answers again, and
// answers whether the room is the event's call. Only what closing ended comes
// back: a link an admin turned off before stays off, and a call whose address
// doesn't answer again (that link, or its address taken since) stays closed,
// so the event can be given a new one. An open call is left as it is; a room
// that is another event's call, or no event call's, is left as it is and
// answered false, so the event comes back without it.
func ReopenEventCall(ctx context.Context, event uuid.UUID, room string) (bool, error) {
	m, err := eventMeeting(ctx, event, room)
	if err != nil || m == nil {
		return false, err
	}
	if m.ArchivedAt == nil {
		return true, nil
	}
	at := *m.ArchivedAt
	err = inTx(ctx, func(ctx context.Context) error {
		if _, err := reviveLinks(ctx, guestModel.ResourceMeeting, room, at); err != nil {
			return err
		}
		links, err := liveLinks(ctx, guestModel.ResourceMeeting, room)
		if err != nil {
			return err
		}
		if addressOf(links) == nil {
			return errStaysClosed
		}
		_, err = reopenMeeting(ctx, m.ID, at)
		return err
	})
	if errors.Is(err, errStaysClosed) || helpers.IsUniqueViolation(err) {
		return true, nil
	}
	return true, err
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
		Make: func(ctx context.Context, event, organiser uuid.UUID, title, room string) (*dgraphStruct.EventCall, error) {
			r, err := EventCall(ctx, event, organiser, title, room)
			if err != nil {
				return nil, err
			}
			// A room it went on keeps its lobby: read the call as its readers do.
			if calls, err := EventCalls(ctx, []string{r.Room}); err == nil && calls[r.Room] != nil {
				return calls[r.Room], nil
			}
			return callOf(r.Room, r.Slug, false), nil
		},
		Ready:  ReadyEventRoom,
		End:    CloseEventCall,
		Calls:  EventCalls,
		Lobby:  callLobbyBusiness.SetInviteesWait,
		Reopen: ReopenEventCall,
	})
}
