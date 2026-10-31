package business

import (
	"context"
	"errors"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// A channel's call room is the channel's uuid.
const (
	channelRoom = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	otherRoom   = "7a1d9e44-2c6b-4f0e-8d3a-5b9c1e2f4a66"
)

// aCall is a conversation's call going on in channelRoom, with Maya hosting,
// Ben a participant and Jonas in by its invite; and what Invite and Enter did.
type aCall struct {
	session              uuid.UUID
	open                 *callStateModel.Session
	people               []callStateBusiness.Person
	links                []uuid.UUID
	admitted             []callStateModel.Admission
	minted               []string
	inConvo, host, guest bool
	doorErr, mintErr     error
	// enterErr is why the call keeps someone out: a lock, or a host's removal.
	enterErr error
	door     guestBusiness.Door
}

func withACall(t *testing.T, kind string) *aCall {
	t.Helper()
	c := &aCall{session: uuid.New(), inConvo: false, guest: true}
	c.open = &callStateModel.Session{ID: c.session, Room: channelRoom, ResourceKind: kind}
	c.people = []callStateBusiness.Person{
		{Identity: "0xmaya", Role: callStateModel.RoleHost, In: true},
		{Identity: "0xben", Role: callStateModel.RoleParticipant, In: true},
		{Identity: "0xjonas", Role: callStateModel.RoleInvitee, In: true},
		{Identity: "0xgone", Role: callStateModel.RoleHost, In: false},
	}
	old := seams
	t.Cleanup(func() { seams = old })
	seams.openSession = func(_ context.Context, room string) (*callStateModel.Session, error) {
		if room == channelRoom {
			return c.open, nil
		}
		return nil, nil
	}
	seams.sessionView = func(_ context.Context, id string) (*callStateBusiness.Session, error) {
		return &callStateBusiness.Session{ID: id, Resource: callStateBusiness.Ref{Kind: kind, ID: "x"}, Live: true, People: c.people}, nil
	}
	seams.newCallLink = func(_ context.Context, session, by uuid.UUID) (*guestBusiness.CallLink, error) {
		if !c.guest {
			return nil, guestBusiness.ErrGuestDisabled
		}
		c.links = append(c.links, session)
		return &guestBusiness.CallLink{GuestToken: "raw", GrantID: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	seams.openDoor = func(context.Context, string) (guestBusiness.Door, error) { return c.door, c.doorErr }
	seams.inConversation = func(context.Context, string, string) (bool, error) { return c.inConvo, nil }
	seams.hostByRule = func(context.Context, string, *dgraphStruct.DgraphUser, bool) (bool, error) { return c.host, nil }
	seams.mayEnter = func(context.Context, string, *dgraphStruct.DgraphUser, bool) error { return c.enterErr }
	seams.admit = func(_ context.Context, a callStateModel.Admission) error {
		c.admitted = append(c.admitted, a)
		return nil
	}
	seams.mintToken = func(_ context.Context, room string, _ *dgraphStruct.DgraphUser, admin bool, _ string) (string, bool, error) {
		if c.mintErr != nil {
			return "", false, c.mintErr
		}
		c.minted = append(c.minted, room)
		return "member-token", true, nil
	}
	seams.title = func(context.Context, string, uuid.UUID) string { return "Maya Chen's call" }
	return c
}

func member(uid string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: uid, Uuid: uuid.NewString()}
}

// Someone in a conversation's call who may let people in makes its invite:
// a host or co-host of a channel's, anyone in a DM's or a group's; never a
// participant of a channel's, someone in by an invite, someone not in the
// call, or anyone while guest access is off (HU13).
func TestWhoMayInviteSomeoneIntoACall(t *testing.T) {
	ctx := context.Background()
	c := withACall(t, "channel")
	if link, err := Invite(ctx, channelRoom, member("0xmaya")); err != nil || link == nil || len(c.links) != 1 || c.links[0] != c.session {
		t.Fatalf("the host: %+v, %v (links %v)", link, err, c.links)
	}
	for who, want := range map[string]error{"0xben": ErrNotAllowed, "0xjonas": ErrNotAllowed, "0xgone": ErrNotInCall, "0xnobody": ErrNotInCall} {
		if _, err := Invite(ctx, channelRoom, member(who)); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", who, err, want)
		}
	}
	if _, err := Invite(ctx, otherRoom, member("0xmaya")); !errors.Is(err, ErrNoCall) {
		t.Errorf("no call going on: %v", err)
	}
	c.guest = false
	if _, err := Invite(ctx, channelRoom, member("0xmaya")); !errors.Is(err, ErrGuestsOff) {
		t.Errorf("guest access off: %v", err)
	}

	c = withACall(t, "dm")
	if link, err := Invite(ctx, channelRoom, member("0xben")); err != nil || link == nil || len(c.links) != 1 || c.links[0] != c.session {
		t.Errorf("anyone in a DM's call: %+v, %v (links %v)", link, err, c.links)
	}
	if _, err := Invite(ctx, channelRoom, member("0xjonas")); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("an invitee in a DM's call: %v", err)
	}

	withACall(t, "meeting")
	if _, err := Invite(ctx, channelRoom, member("0xmaya")); !errors.Is(err, ErrMeetingLink) {
		t.Errorf("a meeting, which has its own link: %v", err)
	}
}

// A member at a door comes in as themselves: through a call's invite to that
// call alone (unless they're in its conversation, when they just join it),
// at a meeting's link or a room's address for as long as it lives. The lock
// and a host's removal still keep them out (HU13).
func TestAMemberAtADoorComesInAsThemselves(t *testing.T) {
	ctx := context.Background()
	c := withACall(t, "channel")
	invite := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceCall, ResourceID: c.session.String(), CreatedBy: uuid.New()}
	c.door = guestBusiness.Door{Grant: invite, Room: channelRoom}
	jonas := member("0xjonas")

	entry, err := Enter(ctx, Door{Token: "raw"}, jonas, "en")
	if err != nil || entry.Token != "member-token" || entry.Room != channelRoom || entry.Title != "Maya Chen's call" || entry.Conversation != nil {
		t.Fatalf("an invitee: %+v, %v", entry, err)
	}
	if len(c.admitted) != 1 || c.admitted[0].Session == nil || *c.admitted[0].Session != c.session ||
		c.admitted[0].GrantID != invite.Id || c.admitted[0].Identity != "0xjonas" || c.admitted[0].Room != channelRoom {
		t.Errorf("the admission, to that call alone: %+v", c.admitted)
	}

	// Someone already in the conversation just joins its call: nothing to admit, no token here.
	c.admitted, c.minted, c.inConvo = nil, nil, true
	entry, err = Enter(ctx, Door{Token: "raw"}, member("0xben"), "en")
	if err != nil || entry.Conversation == nil || entry.Conversation.Kind != "channel" || entry.Token != "" || len(c.admitted) != 0 || len(c.minted) != 0 {
		t.Errorf("a member of the conversation: %+v, %v", entry, err)
	}

	// A meeting's link: admitted for as long as the link lives, the lobby skipped.
	c.inConvo = false
	meeting := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: "meet-m1", CreatedBy: uuid.New()}
	c.door = guestBusiness.Door{Grant: meeting, Room: "meet-m1"}
	c.admitted, c.minted = nil, nil
	if entry, err := Enter(ctx, Door{Token: "raw"}, member("0xcolleague"), "en"); err != nil || entry.Token == "" || len(c.admitted) != 1 || c.admitted[0].Session != nil || c.minted[0] != "meet-m1" {
		t.Errorf("a colleague at a meeting's link: %+v, %v, %+v", entry, err, c.admitted)
	}

	// The lock or a removal says so in its own words, and leaves no admission
	// behind (audit C10): an admission counts someone as maybe in the call.
	c.door = guestBusiness.Door{Grant: invite, Room: channelRoom}
	for _, refusal := range []error{liveKitBusiness.ErrCallLocked, liveKitBusiness.ErrRemovedFromCall} {
		c.enterErr, c.admitted, c.minted = refusal, nil, nil
		if _, err := Enter(ctx, Door{Token: "raw"}, member("0xlate"), "en"); !errors.Is(err, refusal) || len(c.admitted) != 0 || len(c.minted) != 0 {
			t.Errorf("kept out (%v): %v, admitted %+v", refusal, err, c.admitted)
		}
	}
	c.enterErr = nil
	c.doorErr = guestBusiness.ErrInvalidGrant
	if _, err := Enter(ctx, Door{Token: "raw"}, member("0xlate"), "en"); !errors.Is(err, guestBusiness.ErrInvalidGrant) {
		t.Errorf("a door that opens nothing: %v", err)
	}
	if _, err := Enter(ctx, Door{}, member("0xlate"), "en"); !errors.Is(err, guestBusiness.ErrInvalidGrant) {
		t.Errorf("no door at all: %v", err)
	}
}

// A call's invites end with the call, and what they let in is forgotten.
func TestACallsInvitesEndWithIt(t *testing.T) {
	old := seams
	t.Cleanup(func() { seams = old })
	var ended, forgot []uuid.UUID
	seams.endCallLinks = func(_ context.Context, s uuid.UUID) error { ended = append(ended, s); return nil }
	seams.forgetCall = func(_ context.Context, s uuid.UUID) error { forgot = append(forgot, s); return nil }
	session := uuid.New()
	e := resourceEventBusiness.Event{Kind: resourceEventBusiness.CallEnded, Data: map[string]string{"session": session.String()}}
	if !wantsCallEnded(e) || wantsCallEnded(resourceEventBusiness.Event{Kind: resourceEventBusiness.CallStarted}) {
		t.Fatal("the sink takes the wrong events")
	}
	endInvites(context.Background(), e)
	if len(ended) != 1 || ended[0] != session || len(forgot) != 1 || forgot[0] != session {
		t.Errorf("ended %v, forgot %v", ended, forgot)
	}
}
