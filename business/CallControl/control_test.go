package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// Who may do what (D-CL-6): in a DM or a group everyone may mute the others
// and nobody may remove, lock or end; in a channel or a meeting a host may do
// everything, a co-host all but ending the call and naming co-hosts, and a
// participant or a guest nothing to others.
func TestAbilitiesFollowTheCallAndThePart(t *testing.T) {
	for _, c := range []struct {
		kind, role string
		has, lacks []Ability
	}{
		{"dm", model.RoleParticipant, []Ability{CanMute, CanMuteAll, CanAskUnmute, CanActivities}, []Ability{CanRemove, CanLock, CanEnd, CanCohost, CanSpotlight, CanHold}},
		{"group", model.RoleParticipant, []Ability{CanMute, CanMuteAll, CanAskUnmute, CanActivities}, []Ability{CanRemove, CanLock, CanEnd, CanCohost, CanHold}},
		{"group", model.RoleGuest, nil, []Ability{CanMute, CanMuteAll}},
		{"channel", model.RoleHost, everything, nil},
		{"channel", model.RoleCohost, []Ability{CanMute, CanMuteAll, CanRemove, CanLock, CanSpotlight, CanMuteOnEntry, CanHold, CanActivities}, []Ability{CanEnd, CanCohost}},
		{"channel", model.RoleParticipant, nil, everything},
		{"meeting", model.RoleHost, everything, nil},
		{"meeting", model.RoleGuest, nil, everything},
		// Someone in through a call's invite runs nothing, even where everyone is equal (HU13).
		{"dm", model.RoleInvitee, nil, everything},
		{"group", model.RoleInvitee, nil, everything},
		{"channel", model.RoleInvitee, nil, everything},
	} {
		got := AbilitiesOf(c.kind, c.role)
		for _, a := range c.has {
			if !slices.Contains(got, a) {
				t.Errorf("a %s in a %s call can't %s", c.role, c.kind, a)
			}
		}
		for _, a := range c.lacks {
			if slices.Contains(got, a) {
				t.Errorf("a %s in a %s call may %s", c.role, c.kind, a)
			}
		}
	}
}

// What the room is told about someone is their part and exactly what they may
// do, so every screen draws the same controls the server allows.
func TestThePartsAreToldToTheRoom(t *testing.T) {
	host := attributesOf("channel", model.RoleHost)
	if host[AttrRole] != "host" || host[AttrCan] != "mute,mute_all,ask_unmute,remove,lock,end,cohost,spotlight,mute_on_entry,lower_hands,admit,hold,activities,breakouts" {
		t.Errorf("a channel's host is told %v", host)
	}
	guest := attributesOf("meeting", model.RoleGuest)
	if guest[AttrRole] != "guest" || guest[AttrCan] != "" {
		t.Errorf("a guest is told %v", guest)
	}
	if every := attributesOf("dm", model.RoleParticipant); every[AttrCan] != "mute,mute_all,ask_unmute,lower_hands,admit,activities" {
		t.Errorf("someone in a DM's call is told %v", every)
	}
}

func callWith(kind string, people ...callStateBusiness.Person) *callStateBusiness.Session {
	for i := range people {
		people[i].In = true
	}
	return &callStateBusiness.Session{ID: uuid.NewString(), Resource: callStateBusiness.Ref{Kind: kind, ID: "c"}, Live: true, People: people}
}

// Nobody acts on someone above them: a co-host can't put out or mute a host,
// nobody removes or makes a co-host of their equal, a guest is never made a
// co-host, and your own microphone is yours to work.
func TestActionsReachOnlyPeopleTheyMay(t *testing.T) {
	host := callStateBusiness.Person{Identity: "0x1", Name: "Maya", Role: model.RoleHost}
	host2 := callStateBusiness.Person{Identity: "0x4", Name: "Ana", Role: model.RoleHost}
	cohost := callStateBusiness.Person{Identity: "0x2", Name: "Jonas", Role: model.RoleCohost}
	member := callStateBusiness.Person{Identity: "0x3", Name: "Sam", Role: model.RoleParticipant}
	guest := callStateBusiness.Person{Identity: "guest-x", Name: "Guest: Priya", Role: model.RoleGuest, Guest: true}
	s := callWith("channel", host, host2, cohost, member, guest)
	me := func(p callStateBusiness.Person) *callStateBusiness.Person { return s.Person(p.Identity) }

	for _, c := range []struct {
		name   string
		by     callStateBusiness.Person
		action string
		target string
		want   error
	}{
		{"a host removes a participant", host, ActRemove, member.Identity, nil},
		{"a host removes a guest", host, ActRemove, guest.Identity, nil},
		{"a host can't remove another host", host, ActRemove, host2.Identity, ErrAboveYou},
		{"a co-host can't remove a host", cohost, ActRemove, host.Identity, ErrAboveYou},
		{"a co-host can't mute a host", cohost, ActMute, host.Identity, ErrAboveYou},
		{"a host mutes another host", host, ActMute, host2.Identity, nil},
		{"a host can't make a guest a co-host", host, ActMakeCohost, guest.Identity, ErrGuestCohost},
		{"your own microphone is yours", host, ActMute, host.Identity, ErrYourself},
		{"anyone may be spotlit, you too", host, ActSpotlight, host.Identity, nil},
		{"someone not in the call", host, ActMute, "0x99", ErrTargetGone},
		{"nobody named", host, ActRemove, "", ErrNoTarget},
		{"a co-host lets a guest through a hold", cohost, ActAllow, guest.Identity, nil},
		{"nobody lets themselves through", cohost, ActAllow, cohost.Identity, ErrYourself},
	} {
		_, err := targetOf(s, me(c.by), c.by.Identity, Action{Action: c.action, Identity: c.target})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}

	equals := callWith("group", callStateBusiness.Person{Identity: "0x1", Role: model.RoleParticipant}, callStateBusiness.Person{Identity: "0x2", Role: model.RoleParticipant})
	if _, err := targetOf(equals, equals.Person("0x1"), "0x1", Action{Action: ActMute, Identity: "0x2"}); err != nil {
		t.Errorf("in a group's call, equals can't mute each other: %v", err)
	}
}

// Only the actions Call Control knows need an ability; anything else is
// refused before anything is read.
func TestAnUnknownActionIsRefusedFirst(t *testing.T) {
	if _, err := Act(context.Background(), nil, uuid.NewString(), Action{Action: "unmute_everyone"}); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("an unknown action: %v", err)
	}
	for _, a := range []string{ActMute, ActMuteAll, ActAskUnmute, ActAskAllUnmute, ActRemove, ActLock, ActUnlock, ActEnd, ActMakeCohost, ActRemoveCohost, ActSpotlight, ActEndSpotlight, ActMuteOnEntry, ActLowerHand, ActLowerAll, ActAdmit, ActDeny, ActAdmitAll, ActHold, ActAllow} {
		if abilityFor(a) == "" {
			t.Errorf("%s needs no ability", a)
		}
	}
}

// A mute turns off what it names, and only that: a screen share takes its
// sound with it.
func TestAMuteTurnsOffWhatItNames(t *testing.T) {
	if got := sourcesOf(""); !slices.Equal(got, []livekit.TrackSource{livekit.TrackSource_MICROPHONE}) {
		t.Errorf("by default: %v", got)
	}
	if got := sourcesOf("camera"); !slices.Equal(got, []livekit.TrackSource{livekit.TrackSource_CAMERA}) {
		t.Errorf("camera: %v", got)
	}
	if got := sourcesOf("screen_share"); !slices.Equal(got, []livekit.TrackSource{livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO}) {
		t.Errorf("screen share: %v", got)
	}
	if sourceName("anything") != "microphone" || sourceName("camera") != "camera" {
		t.Error("the packet names the wrong source")
	}
}

// The part someone takes when they join, where it needs nobody looked up: a
// guest is a guest, in a DM or a group everyone is equal, and whoever starts a
// channel's call hosts it.
func TestThePartSomeoneTakesWhenTheyJoin(t *testing.T) {
	ctx := context.Background()
	member := uuid.New()
	join := func(id *uuid.UUID) callStateBusiness.JoinEvent {
		return callStateBusiness.JoinEvent{Room: "r", Identity: "0x1", UserUUID: id, At: time.Now()}
	}
	oldIn := inConversation
	t.Cleanup(func() { inConversation = oldIn })
	in := true
	asked := 0
	inConversation = func(_ context.Context, room, uid string) (bool, error) {
		asked++
		if room != "r" || uid != "0x1" {
			t.Errorf("asked about %s in %s", uid, room)
		}
		return in, nil
	}
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "channel", ID: "c"}, join(nil), true); got != model.RoleGuest {
		t.Errorf("a guest starting a call takes %q", got)
	}
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "group", ID: "g"}, join(&member), true); got != model.RoleParticipant {
		t.Errorf("the first into a group's call takes %q", got)
	}
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "channel", ID: "c"}, join(&member), true); got != model.RoleHost {
		t.Errorf("whoever starts a channel's call takes %q", got)
	}

	// A member let in through a call's invite isn't in its conversation: an
	// invitee, in a DM, a group or a channel (HU13). A check that can't be
	// answered gives the part with no powers.
	in = false
	for _, kind := range []string{"dm", "group", "channel"} {
		if got := roleRule(ctx, callStateBusiness.Ref{Kind: kind, ID: "x"}, join(&member), false); got != model.RoleInvitee {
			t.Errorf("an invitee in a %s's call takes %q", kind, got)
		}
	}
	inConversation = func(context.Context, string, string) (bool, error) { return false, errors.New("down") }
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "group", ID: "g"}, join(&member), false); got != model.RoleInvitee {
		t.Errorf("unchecked, someone in a group's call takes %q", got)
	}
	// A meeting is in no conversation: nobody there is asked.
	asked = 0
	roleRule(ctx, callStateBusiness.Ref{Kind: "meeting", ID: "m"}, join(nil), false)
	if asked != 0 {
		t.Error("a meeting's guest was asked about a conversation")
	}
}

// Only identities this server mints are judged when someone connects;
// LiveKit's own participants (the recorder) are none of its business. A guest
// token only ever names an instant meeting.
func TestWhoIsJudgedWhenTheyConnect(t *testing.T) {
	ctx := context.Background()
	if ok, _ := mayStay(ctx, "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", "EG_abc123"); !ok {
		t.Error("the recorder was put out of a call")
	}
	ok, reason := mayStay(ctx, "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", "guest-6f1c0e2a-1111-4222-8333-944455556666-abcdefgh")
	if ok || reason != liveKitBusiness.RemovedLinkRevoked {
		t.Errorf("a guest in a channel's call: stays %v (%s)", ok, reason)
	}
}

// Every refusal says what to do next, in words, and never as a server error.
func TestRefusalsSayWhatToDo(t *testing.T) {
	for _, r := range []*Refusal{ErrUnknownAction, ErrNoSuchFeedback, ErrHandDown, ErrNotWaiting, ErrNoTarget, ErrThroughToken, ErrNotInCall, ErrNotAllowed, ErrAboveYou, ErrYourself, ErrGuestCohost, ErrTargetGone, ErrCallOver} {
		if r.Status < 400 || r.Status >= 500 || r.Code == "" || !strings.HasSuffix(r.Msg, ".") {
			t.Errorf("%+v", r)
		}
	}
}

// Hands (CALLS-3a): your own hand and feedback are yours, guests included,
// and need no ability; lowering someone else's hand, or everyone's, needs
// lower_hands, which the people running a call have, and everyone in a DM or
// a group.
func TestHandsAreYoursAndLoweringOthersIsForWhoRunsTheCall(t *testing.T) {
	me := "0xme"
	for _, c := range []struct {
		a    Action
		want bool
	}{
		{Action{Action: ActRaiseHand}, true},
		{Action{Action: ActSetFeedback, Feedback: "yes"}, true},
		{Action{Action: ActLowerHand}, true},
		{Action{Action: ActLowerHand, Identity: me}, true},
		{Action{Action: ActLowerHand, Identity: "0xother"}, false},
		{Action{Action: ActLowerAll}, false},
		{Action{Action: ActMute, Identity: me}, false},
	} {
		if got := OwnAction(c.a, me); got != c.want {
			t.Errorf("OwnAction(%+v) = %v, want %v", c.a, got, c.want)
		}
	}
	for _, c := range []struct {
		kind, role string
		may        bool
	}{
		{"channel", model.RoleHost, true},
		{"channel", model.RoleCohost, true},
		{"channel", model.RoleParticipant, false},
		{"meeting", model.RoleGuest, false},
		{"dm", model.RoleParticipant, true},
		{"group", model.RoleParticipant, true},
		{"group", model.RoleGuest, false},
	} {
		if got := slices.Contains(AbilitiesOf(c.kind, c.role), CanLowerHands); got != c.may {
			t.Errorf("%s %s may lower hands: %v, want %v", c.kind, c.role, got, c.may)
		}
	}
	if !targeted(ActLowerHand) || targeted(ActLowerAll) || targeted(ActRaiseHand) {
		t.Error("only lowering one hand names whose")
	}
}

// What the room is told of someone as they (re)join carries their hand and
// feedback, so a reconnect keeps both; a hand that is down is "" (taken away).
func TestARejoinTellsTheRoomTheHandAndFeedback(t *testing.T) {
	at := time.UnixMilli(1760900000123)
	got := personAttributes("channel", &callStateBusiness.Person{Identity: "0xa", Role: model.RoleParticipant, HandRaisedAt: &at, Feedback: "slow_down"})
	if got[AttrHand] != "1760900000123" || got[AttrFeedback] != "slow_down" || got[AttrRole] != model.RoleParticipant {
		t.Errorf("attributes %v", got)
	}
	down := personAttributes("channel", &callStateBusiness.Person{Identity: "0xa", Role: model.RoleHost})
	if v, ok := down[AttrHand]; !ok || v != "" {
		t.Errorf("a hand that is down must be taken away: %q, %v", v, ok)
	}
	if v, ok := down[AttrFeedback]; !ok || v != "" {
		t.Errorf("no feedback must be taken away: %q, %v", v, ok)
	}
	if v, ok := down[AttrAllowed]; !ok || v != "" {
		t.Errorf("nothing let through must be taken away: %q, %v", v, ok)
	}
	let := personAttributes("channel", &callStateBusiness.Person{Identity: "0xa", Role: model.RoleParticipant, Allowed: []string{model.HoldMicrophone, model.HoldShare}})
	if let[AttrAllowed] != "microphone,share" {
		t.Errorf("what a host let them do: %q", let[AttrAllowed])
	}
}

// Whom a hold holds (calls meet-12): everyone but the people running the call
// and whoever they let through, and nobody while it isn't held.
func TestAHoldHoldsWhoeverDoesntRunTheCall(t *testing.T) {
	s := callWith("channel",
		callStateBusiness.Person{Identity: "0xh", Role: model.RoleHost},
		callStateBusiness.Person{Identity: "0xc", Role: model.RoleCohost},
		callStateBusiness.Person{Identity: "0xp", Role: model.RoleParticipant},
		callStateBusiness.Person{Identity: "0xl", Role: model.RoleParticipant, Allowed: []string{model.HoldMicrophone}},
		callStateBusiness.Person{Identity: "guest-a-b", Role: model.RoleGuest, Guest: true},
	)
	s.Held = []string{model.HoldMicrophone}
	for identity, want := range map[string]bool{"0xh": true, "0xc": true, "0xp": false, "0xl": true, "guest-a-b": false} {
		if got := mayDo(s, s.Person(identity), model.HoldMicrophone); got != want {
			t.Errorf("%s may turn on their microphone: %v, want %v", identity, got, want)
		}
		if !mayDo(s, s.Person(identity), model.HoldCamera) {
			t.Errorf("%s is held from what the call doesn't hold", identity)
		}
	}
}

// A guest is who their own call token says, when this server signed it for a
// guest; a member's token, another server's or a made-up one says nobody.
func TestAGuestIsWhoTheirCallTokenSays(t *testing.T) {
	was := livekitInit.LiveKitService.Config
	livekitInit.LiveKitService.Config = &livekitInit.LiveKitConfigStruct{ApiKey: "devkey", ApiSecret: "a-secret-long-enough-for-hs256-signing"}
	t.Cleanup(func() { livekitInit.LiveKitService.Config = was })
	mint := func(key, secret, identity, room string) string {
		at := lka.NewAccessToken(key, secret)
		at.SetIdentity(identity).SetVideoGrant(&lka.VideoGrant{RoomJoin: true, Room: room}).SetValidFor(time.Hour)
		tok, err := at.ToJWT()
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	id, room, err := GuestOf("Bearer " + mint("devkey", "a-secret-long-enough-for-hs256-signing", "guest-gia", "meet-1"))
	if err != nil || id != "guest-gia" || room != "meet-1" {
		t.Fatalf("a guest's token: %q %q %v", id, room, err)
	}
	for name, tok := range map[string]string{
		"a member's token":  mint("devkey", "a-secret-long-enough-for-hs256-signing", "0x1", "meet-1"),
		"another key":       mint("otherkey", "a-secret-long-enough-for-hs256-signing", "guest-gia", "meet-1"),
		"another secret":    mint("devkey", "not-our-secret-but-long-enough-to-sign", "guest-gia", "meet-1"),
		"no room":           mint("devkey", "a-secret-long-enough-for-hs256-signing", "guest-gia", ""),
		"nothing":           "",
		"something made up": "not-a-token",
	} {
		if _, _, err := GuestOf(tok); !errors.Is(err, ErrBadCallToken) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

// The lobby (CALLS-2): whoever runs a call lets people in or turns them away,
// and in a DM's or a group's call everyone may; a participant of a channel's
// call or a guest may not.
func TestTheLobbyIsAnsweredByWhoRunsTheCall(t *testing.T) {
	for _, c := range []struct {
		kind, role string
		may        bool
	}{
		{"meeting", model.RoleHost, true},
		{"channel", model.RoleCohost, true},
		{"channel", model.RoleParticipant, false},
		{"meeting", model.RoleGuest, false},
		{"group", model.RoleParticipant, true},
	} {
		if got := slices.Contains(AbilitiesOf(c.kind, c.role), CanAdmit); got != c.may {
			t.Errorf("%s %s may admit: %v, want %v", c.kind, c.role, got, c.may)
		}
	}
	for _, a := range []string{ActAdmit, ActDeny, ActAdmitAll} {
		if abilityFor(a) != CanAdmit {
			t.Errorf("%s needs %q", a, abilityFor(a))
		}
	}
}
