// Package business (CallInvite) is a call's invite and the door members come
// in by (calls HU13, CALLS-2), on the one outside-access mechanism (guest
// grants) and the one policy (guest access):
//
//   - Invite: someone who may let people into a conversation's call makes a
//     link that opens that call alone, for as long as it goes on. A guest who
//     opens it knocks and waits; a member who opens it signed in comes in as
//     themselves, in the call only, never its conversation or its name.
//   - Enter: a member at any door (a meeting's link, a room's address, a
//     call's invite) comes in as themselves, skipping the lobby (D-CL-5). At
//     a call's invite to a conversation they're in, they just join its call.
//
// Chat's ring (P10) will call Invite, and never mint access of its own.
//
// IMPORTS. This package sits above business/Guest, CallState, CallControl,
// CallLobby, CallRoom and LiveKit; none of them imports it.
package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// The refusals, in the words the person reads.
var (
	ErrNoCall      = &helpers.CallRefusal{Status: http.StatusNotFound, Code: "no_call", Msg: "There's no call going on here."}
	ErrMeetingLink = &helpers.CallRefusal{Status: http.StatusConflict, Code: "meeting_link", Msg: "A meeting has its own link. Share that instead."}
	ErrNotInCall   = &helpers.CallRefusal{Code: "not_in_call", Msg: "Join the call first."}
	ErrNotAllowed  = &helpers.CallRefusal{Code: "not_allowed", Msg: "Only the people running this call can invite people."}
	ErrGuestsOff   = &helpers.CallRefusal{Code: "guests_off", Msg: "Guest access is off for this workspace."}
)

// seams are the reads and writes this package makes, for tests.
var seams = struct {
	openSession    func(ctx context.Context, room string) (*callStateModel.Session, error)
	sessionView    func(ctx context.Context, id string) (*callStateBusiness.Session, error)
	newCallLink    func(ctx context.Context, session, by uuid.UUID) (*guestBusiness.CallLink, error)
	openDoor       func(ctx context.Context, rawToken string) (guestBusiness.Door, error)
	enterRoom      func(ctx context.Context, slug, unlock string) (*guestModel.GuestGrant, error)
	doorOf         func(ctx context.Context, g *guestModel.GuestGrant) (guestBusiness.Door, error)
	inConversation func(ctx context.Context, room, uid string) (bool, error)
	hostByRule     func(ctx context.Context, room string, user *dgraphStruct.DgraphUser, workspaceAdmin bool) (bool, error)
	mayEnter       func(ctx context.Context, room string, user *dgraphStruct.DgraphUser, meetingHost bool) error
	admit          func(ctx context.Context, a callStateModel.Admission) error
	mintToken      func(ctx context.Context, room string, user *dgraphStruct.DgraphUser, isAdmin bool, language string) (string, bool, error)
	title          func(ctx context.Context, room string, owner uuid.UUID) string
	endCallLinks   func(ctx context.Context, session uuid.UUID) error
	forgetCall     func(ctx context.Context, session uuid.UUID) error
}{
	openSession:    callStateModel.OpenSessionOf,
	sessionView:    callStateBusiness.Get,
	newCallLink:    guestBusiness.NewCallLink,
	openDoor:       guestBusiness.OpenDoor,
	enterRoom:      callRoomBusiness.Enter,
	doorOf:         guestBusiness.DoorOf,
	inConversation: liveKitBusiness.MayOpenConversation,
	hostByRule:     liveKitBusiness.HostByRule,
	mayEnter:       liveKitBusiness.MayEnter,
	admit:          callStateModel.Admit,
	mintToken:      liveKitBusiness.CreateRoomAndGetToken,
	title:          callLobbyBusiness.Title,
	endCallLinks:   guestBusiness.EndCallLinks,
	forgetCall:     callStateModel.ForgetAdmissionsTo,
}

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "calls.invites",
		Wants: wantsCallEnded,
		Run:   endInvites,
	})
}

// Invite makes an invite into the conversation's call going on in a room, for
// someone in it who may let people in (the admit ability: a channel's hosts
// and co-hosts, everyone in a DM or a group, never someone in by an invite).
// A meeting has its own link.
func Invite(ctx context.Context, room string, by *dgraphStruct.DgraphUser) (*guestBusiness.CallLink, error) {
	open, err := seams.openSession(ctx, room)
	if err != nil {
		return nil, err
	}
	if open == nil {
		return nil, ErrNoCall
	}
	if open.ResourceKind == callStateModel.ResourceMeeting {
		return nil, ErrMeetingLink
	}
	s, err := seams.sessionView(ctx, open.ID.String())
	if err != nil {
		return nil, err
	}
	if s == nil || !s.Live {
		return nil, ErrNoCall
	}
	p := s.Person(by.Uid)
	if p == nil || !p.In {
		return nil, ErrNotInCall
	}
	if !slices.Contains(callControlBusiness.AbilitiesOf(s.Resource.Kind, p.Role), callControlBusiness.CanAdmit) {
		return nil, ErrNotAllowed
	}
	byID, err := uuid.Parse(by.Uuid)
	if err != nil {
		return nil, ErrNotInCall
	}
	link, err := seams.newCallLink(ctx, open.ID, byID)
	if errors.Is(err, guestBusiness.ErrGuestDisabled) {
		return nil, ErrGuestsOff
	}
	return link, err
}

// Door is what a member opens: a link's token, or a room's address with the
// unlock its passcode gave.
type Door struct {
	Token  string `json:"token"`
	Slug   string `json:"slug"`
	Unlock string `json:"unlock"`
}

// Entry is a member's way into the call a door opens: their own call token,
// or, at a call's invite to a conversation they're in, that conversation's
// call, which they join as they always do.
type Entry struct {
	Token string `json:"token,omitempty"`
	Room  string `json:"room"`
	// Title is what the call is called for them: "Maya Chen's call", "Maya
	// Chen's room" or a meeting's own title, never a conversation's name.
	Title string `json:"title"`
	// Conversation is set when they're in the call's conversation.
	Conversation *callStateBusiness.Ref `json:"conversation,omitempty"`
}

// Enter lets a member in by a door as themselves. The lock and a host's
// removal still keep them out, in their own words (helpers.CallRefusal).
func Enter(ctx context.Context, d Door, user *dgraphStruct.DgraphUser, language string) (*Entry, error) {
	door, err := openDoor(ctx, d)
	if err != nil {
		return nil, err
	}
	userID, err := uuid.Parse(user.Uuid)
	if err != nil {
		return nil, guestBusiness.ErrInvalidGrant
	}
	title := seams.title(ctx, door.Room, door.Grant.CreatedBy)
	admission := callStateModel.Admission{Room: door.Room, UserUUID: userID, Identity: user.Uid, GrantID: door.Grant.Id, At: time.Now()}
	host := false
	switch door.Grant.ResourceType {
	case guestModel.ResourceCall:
		in, err := seams.inConversation(ctx, door.Room, user.Uid)
		if err != nil {
			return nil, err
		}
		if in {
			ref, _, _ := callStateBusiness.RefOfRoom(door.Room)
			return &Entry{Room: door.Room, Title: title, Conversation: &ref}, nil
		}
		// That call alone: the invite's session.
		session, err := uuid.Parse(door.Grant.ResourceID)
		if err != nil {
			return nil, guestBusiness.ErrInvalidGrant
		}
		admission.Session = &session
	default:
		// A meeting's own host who opened its link runs it, as on its page.
		if host, err = seams.hostByRule(ctx, door.Room, user, false); err != nil {
			return nil, err
		}
	}
	// Kept out first (a lock, or a host's removal), so a refused door leaves no
	// admission behind: an admission counts someone as maybe in the call
	// (audit C10). The token's mint asks again.
	if err := seams.mayEnter(ctx, door.Room, user, host); err != nil {
		return nil, err
	}
	if err := seams.admit(ctx, admission); err != nil {
		return nil, err
	}
	token, _, err := seams.mintToken(ctx, door.Room, user, host, language)
	if err != nil {
		return nil, err
	}
	return &Entry{Token: token, Room: door.Room, Title: title}, nil
}

// openDoor is the call a member's door opens: a link's token, or a room's
// address and its unlock.
func openDoor(ctx context.Context, d Door) (guestBusiness.Door, error) {
	switch {
	case strings.TrimSpace(d.Token) != "":
		return seams.openDoor(ctx, d.Token)
	case strings.TrimSpace(d.Slug) != "":
		g, err := seams.enterRoom(ctx, d.Slug, d.Unlock)
		if err != nil {
			return guestBusiness.Door{}, err
		}
		return seams.doorOf(ctx, g)
	}
	return guestBusiness.Door{}, guestBusiness.ErrInvalidGrant
}

// wantsCallEnded is the one event the invites' sink takes. Pure.
func wantsCallEnded(e resourceEventBusiness.Event) bool {
	return e.Kind == resourceEventBusiness.CallEnded
}

// endInvites ends a call's invites as the call ends, and forgets who came in
// by them, so the admin's list shows only live links. Every check also asks
// that the call goes on: this is tidiness, not the guard.
func endInvites(ctx context.Context, e resourceEventBusiness.Event) {
	session, err := uuid.Parse(e.Data["session"])
	if err != nil {
		return
	}
	if err := seams.endCallLinks(ctx, session); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallInvite could not end the invites of call %s: %+v", session, err)
	}
	if err := seams.forgetCall(ctx, session); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallInvite could not forget who came into call %s: %+v", session, err)
	}
}
