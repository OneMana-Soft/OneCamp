// Package business (CallRoom) is a member's personal meeting room
// (design/calls-vs-zoom/PROGRAM.md, CALLS-2 and D-CL-35): one per person,
// reusable, at their own address ("/r/maya-chen"), until they close it, an
// admin turns its link off, or rooms are turned off.
//
// It is built of what exists. The room is a meeting (call_meetings, kind
// personal) with a lobby like any meeting's (business/CallLobby). Who may come
// in from outside is one standing guest grant published at the owner's address
// (business/Guest NewRoom and PublishLink, the kind table's KindRoom), under
// the guest-access switch and its "Personal meeting rooms" sub-option, and
// never capped by the workspace's longest link lifetime. Its owner hosts it
// from the meeting's page, as they host an instant meeting (/app/meet/m/<id>);
// its guests come in at its address (controllers/Guest's room routes), knock
// and wait as at any meeting.
//
// IMPORTS. One way: this package imports business/Guest, business/CallLobby
// and the meetings' model; the controllers import this one.
package business

import (
	"context"
	"errors"
	"time"

	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

var (
	// ErrNoRoom is a member with no room to change or close.
	ErrNoRoom = errors.New("callroom: no room")
	// ErrNoAddress is a room whose link an admin turned off: opening it again
	// gives it an address.
	ErrNoAddress = errors.New("callroom: the room has no address")
	// ErrLocked is a room with a passcode, asked without its unlock.
	ErrLocked = errors.New("callroom: the room has a passcode")
	// ErrWrongPasscode is a passcode that doesn't open the room.
	ErrWrongPasscode = errors.New("callroom: that passcode doesn't open the room")
	// ErrBusy is an address past its passcode tries.
	ErrBusy = errors.New("callroom: too many tries")
)

// Seams for tests: the meetings, the room's links, the switches and the clock.
var (
	personalMeetingOf = model.PersonalMeetingOf
	saveMeeting       = model.SaveMeeting
	archiveMeeting    = model.ArchiveMeeting
	setLobby          = callLobbyBusiness.SetLobby
	title             = callLobbyBusiness.Title
	liveLinks         = guestModel.ListActiveForResource
	revokeLinks       = guestModel.RevokeLiveForResource
	newRoom           = guestBusiness.NewRoom
	publishLink       = guestBusiness.PublishLink
	removeGuests      = guestBusiness.RemoveMeetingGuests
	resolve           = guestBusiness.ResolveExternal
	unlocks           = guestBusiness.Unlocks
	mintUnlock        = guestBusiness.MintUnlock
	tryAllowed        = guestBusiness.UnlockTryAllowed
	inTx              = pgexec.InTx
	guestOn           = settingsBusiness.GuestAccessEnabled
	roomsAllowed      = settingsBusiness.GuestRoomsAllowed
	roomsOn           = settingsBusiness.GuestRoomsEnabled
	now               = time.Now
)

// Room is a member's personal room, as its owner sees it.
type Room struct {
	MeetingID string `json:"meeting_id"`
	// Room is the call's room ("meet-<meeting id>").
	Room  string `json:"room"`
	Title string `json:"title"`
	// Slug and Address are where its guests come in ("maya-chen",
	// "/r/maya-chen"); both empty while an admin has turned its link off.
	Slug    string `json:"slug"`
	Address string `json:"address"`
	// Passcode is whether a guest needs one to come in.
	Passcode bool `json:"passcode"`
	// GuestsWait is whether guests wait in the lobby to be let in (the default).
	GuestsWait bool `json:"guests_wait"`
	// On is whether its address answers now: guest access and personal rooms
	// are both on.
	On bool `json:"on"`
}

// roomOf is a meeting's call room. Pure.
func roomOf(m *model.Meeting) string { return guestBusiness.MeetingRoomPrefix + m.ID }

// addressOf is a room's link at its address among a room's live links, nil
// for none. Pure.
func addressOf(links []*guestModel.GuestGrant) *guestModel.GuestGrant {
	if g, _ := guestModel.StandingOf(links); guestBusiness.KindRoom.Serves(g) {
		return g
	}
	return nil
}

// describe is a room as its owner sees it.
func describe(ctx context.Context, m *model.Meeting) (*Room, error) {
	room := roomOf(m)
	links, err := liveLinks(ctx, guestModel.ResourceMeeting, room)
	if err != nil {
		return nil, err
	}
	r := &Room{MeetingID: m.ID, Room: room, Title: title(ctx, room, m.Owner), GuestsWait: m.Lobby != model.LobbyNobody, On: roomsOn()}
	if g := addressOf(links); g != nil {
		r.Slug = *g.Slug
		r.Address = guestBusiness.KindRoom.WebAddress(r.Slug)
		r.Passcode = g.PasswordHash != nil
	}
	return r, nil
}

// Mine is a member's room, nil when they have none.
func Mine(ctx context.Context, owner uuid.UUID) (*Room, error) {
	m, err := personalMeetingOf(ctx, owner)
	if err != nil || m == nil {
		return nil, err
	}
	return describe(ctx, m)
}

// Open gives a member their room: the one they have, given an address again
// if an admin turned its link off, or a new one, its meeting and its link at
// their name made in one transaction. Two Opens at once give the same room:
// the second's meeting is refused by the one-room index (migration
// *_call_rooms), and it answers the first's. Two people of one name opening
// at once both find the bare name free, and the second's publish is refused,
// which ends its transaction: it tries once more in a fresh one, which finds
// the name taken and adds letters (reviewed by DOCS-LEAD-2, 20 Oct).
func Open(ctx context.Context, owner uuid.UUID) (*Room, error) {
	if !guestOn() {
		return nil, guestBusiness.ErrGuestDisabled
	}
	if !roomsAllowed() {
		return nil, guestBusiness.ErrRoomsOff
	}
	m, err := open(ctx, owner)
	if errors.Is(err, guestBusiness.ErrSlugTaken) {
		m, err = open(ctx, owner)
	}
	if helpers.IsUniqueViolation(err) {
		return Mine(ctx, owner)
	}
	if err != nil {
		return nil, err
	}
	return describe(ctx, m)
}

// open is one try at Open, in one transaction: the member's meeting, made
// when they have none, and its link at an address, made when it has none.
func open(ctx context.Context, owner uuid.UUID) (*model.Meeting, error) {
	var m *model.Meeting
	err := inTx(ctx, func(ctx context.Context) error {
		var err error
		if m, err = personalMeetingOf(ctx, owner); err != nil {
			return err
		}
		if m == nil {
			m = &model.Meeting{ID: uuid.NewString(), Kind: model.KindPersonal, Owner: owner, Lobby: model.LobbyGuests, CreatedAt: now()}
			if err := saveMeeting(ctx, *m); err != nil {
				return err
			}
		}
		links, err := liveLinks(ctx, guestModel.ResourceMeeting, roomOf(m))
		if err != nil || addressOf(links) != nil {
			return err
		}
		_, err = newRoom(ctx, roomOf(m), owner)
		return err
	})
	return m, err
}

// Changes are what a room's owner changes, each only when sent.
type Changes struct {
	// Slug is a new address; the old one stops answering at once, and every
	// passcode unlock given for it ends.
	Slug *string `json:"slug"`
	// Passcode sets a passcode, or with "" takes it off; a new one ends every
	// unlock given before.
	Passcode *string `json:"passcode"`
	// GuestsWait is whether guests wait in the lobby (true) or anyone with
	// the address comes in at once.
	GuestsWait *bool `json:"guests_wait"`
}

// InvalidChange is a change that can't be made, saying why.
type InvalidChange struct{ Reason string }

func (e *InvalidChange) Error() string { return "callroom: " + e.Reason }

// Change changes a member's room, all of it or none (one transaction).
// ErrNoRoom when they have none; ErrNoAddress for an address or passcode on
// a room whose link an admin turned off; business/Guest's ErrSlugTaken,
// PublishInvalid, ErrRoomsOff and ErrGuestDisabled as publishing answers them.
func Change(ctx context.Context, owner uuid.UUID, in Changes) (*Room, error) {
	if in.Passcode != nil && *in.Passcode != "" {
		if why := guestBusiness.SecretProblem("passcode", *in.Passcode); why != "" {
			return nil, &InvalidChange{why}
		}
	}
	m, err := personalMeetingOf(ctx, owner)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNoRoom
	}
	err = inTx(ctx, func(ctx context.Context) error {
		if in.Slug != nil || in.Passcode != nil {
			links, err := liveLinks(ctx, guestModel.ResourceMeeting, roomOf(m))
			if err != nil {
				return err
			}
			g := addressOf(links)
			if g == nil {
				return ErrNoAddress
			}
			set := guestBusiness.PublishSettings{Password: in.Passcode}
			if in.Slug != nil {
				set.Slug = *in.Slug
			}
			if _, err := publishLink(ctx, owner, g.Id, set); err != nil {
				return err
			}
		}
		if in.GuestsWait != nil {
			if _, err := setLobby(ctx, m.ID, *in.GuestsWait); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Mine(ctx, owner)
}

// Close ends a member's room: its address stops answering, every link to it
// with it, and its meeting is closed for good, in one transaction; then its
// guests are put out of its call. Opening again makes a new room, at their
// name again when it is free.
func Close(ctx context.Context, owner uuid.UUID) error {
	m, err := personalMeetingOf(ctx, owner)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNoRoom
	}
	return closeMeeting(ctx, m)
}

// closeMeeting ends a room's meeting, a person's or an event's: its links end
// and the meeting is closed for good, in one transaction; then its guests are
// put out of its call.
func closeMeeting(ctx context.Context, m *model.Meeting) error {
	err := inTx(ctx, func(ctx context.Context) error {
		if _, err := revokeLinks(ctx, guestModel.ResourceMeeting, roomOf(m)); err != nil {
			return err
		}
		_, err := archiveMeeting(ctx, m.ID, now())
		return err
	})
	if err != nil {
		return err
	}
	removeGuests(ctx, roomOf(m))
	return nil
}

// At is what a room's address answers now for someone holding unlock (its
// passcode's unlock, or ""): its state by the one rule every kind's address
// answers by (business/Guest StateFor: live, locked, gone, missing), live
// once unlock opens a room with a passcode, and the room's link while live.
// One resolve, so the state and the link never disagree.
func At(ctx context.Context, slug, unlock string) (string, *guestModel.GuestGrant, error) {
	g, err := resolve(ctx, guestBusiness.External{Slug: slug})
	state, err := guestBusiness.StateFor(guestBusiness.KindRoom, g, err)
	if err != nil {
		return "", nil, err
	}
	if state == guestBusiness.StateLocked && unlocks(unlock, g, now()) {
		state = guestBusiness.StateLive
	}
	if state != guestBusiness.StateLive {
		return state, nil, nil
	}
	return state, g, nil
}

// Enter is a room's link for someone at its address while it answers:
// ErrLocked for a room with a passcode they haven't unlocked, and
// business/Guest's uniform not-available (ErrInvalidGrant) for any other
// state but live.
func Enter(ctx context.Context, slug, unlock string) (*guestModel.GuestGrant, error) {
	state, g, err := At(ctx, slug, unlock)
	switch {
	case err != nil:
		return nil, err
	case state == guestBusiness.StateLocked:
		return nil, ErrLocked
	case g == nil:
		return nil, guestBusiness.ErrInvalidGrant
	}
	return g, nil
}

// Unlock is a room's unlock for its passcode, tried from ip: the token, and
// how long it opens the room. The address's tries are counted first, per
// room, whichever kind's address they were tried at (business/Guest).
func Unlock(ctx context.Context, slug, passcode, ip string) (string, time.Duration, error) {
	g, err := resolve(ctx, guestBusiness.External{Slug: slug})
	if err != nil {
		return "", 0, err
	}
	if !guestBusiness.KindRoom.Serves(g) {
		return "", 0, guestBusiness.ErrInvalidGrant
	}
	if !tryAllowed(ctx, helpers.RateKeyIP(ip), g) {
		return "", 0, ErrBusy
	}
	if !guestBusiness.PasswordOpens(g, passcode) {
		return "", 0, ErrWrongPasscode
	}
	token, lasts := mintUnlock(g, now())
	if token == "" {
		return "", 0, errors.New("callroom: the server has no signing secret")
	}
	return token, lasts, nil
}
