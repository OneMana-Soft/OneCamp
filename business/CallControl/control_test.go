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
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
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
		{"dm", model.RoleParticipant, []Ability{CanMute, CanMuteAll, CanAskUnmute}, []Ability{CanRemove, CanLock, CanEnd, CanCohost, CanSpotlight}},
		{"group", model.RoleParticipant, []Ability{CanMute, CanMuteAll, CanAskUnmute}, []Ability{CanRemove, CanLock, CanEnd, CanCohost}},
		{"group", model.RoleGuest, nil, []Ability{CanMute, CanMuteAll}},
		{"channel", model.RoleHost, everything, nil},
		{"channel", model.RoleCohost, []Ability{CanMute, CanMuteAll, CanRemove, CanLock, CanSpotlight, CanMuteOnEntry}, []Ability{CanEnd, CanCohost}},
		{"channel", model.RoleParticipant, nil, everything},
		{"meeting", model.RoleHost, everything, nil},
		{"meeting", model.RoleGuest, nil, everything},
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
	if host[AttrRole] != "host" || host[AttrCan] != "mute,mute_all,ask_unmute,remove,lock,end,cohost,spotlight,mute_on_entry" {
		t.Errorf("a channel's host is told %v", host)
	}
	guest := attributesOf("meeting", model.RoleGuest)
	if guest[AttrRole] != "guest" || guest[AttrCan] != "" {
		t.Errorf("a guest is told %v", guest)
	}
	if every := attributesOf("dm", model.RoleParticipant); every[AttrCan] != "mute,mute_all,ask_unmute" {
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
	for _, a := range []string{ActMute, ActMuteAll, ActAskUnmute, ActAskAllUnmute, ActRemove, ActLock, ActUnlock, ActEnd, ActMakeCohost, ActRemoveCohost, ActSpotlight, ActEndSpotlight, ActMuteOnEntry} {
		if abilityFor(a) == "" {
			t.Errorf("%s needs no ability", a)
		}
	}
}

// The transcription agent and LiveKit's hidden recorder are not people: they
// are never recorded in a call, never handed it, never muted with everyone.
func TestOnlyPeopleCount(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *livekit.ParticipantInfo
		want bool
	}{
		{"a member", &livekit.ParticipantInfo{Identity: "0x1"}, true},
		{"a guest", &livekit.ParticipantInfo{Identity: "guest-a-b"}, true},
		{"the transcription agent", &livekit.ParticipantInfo{Identity: "transcriber-bot"}, false},
		{"a hidden recorder", &livekit.ParticipantInfo{Identity: "EG_1", Permission: &livekit.ParticipantPermission{Hidden: true}}, false},
		{"an egress", &livekit.ParticipantInfo{Identity: "EG_2", Kind: livekit.ParticipantInfo_EGRESS}, false},
		{"nobody", nil, false},
	} {
		if got := human(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
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
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "channel", ID: "c"}, join(nil), true); got != model.RoleGuest {
		t.Errorf("a guest starting a call takes %q", got)
	}
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "group", ID: "g"}, join(&member), true); got != model.RoleParticipant {
		t.Errorf("the first into a group's call takes %q", got)
	}
	if got := roleRule(ctx, callStateBusiness.Ref{Kind: "channel", ID: "c"}, join(&member), true); got != model.RoleHost {
		t.Errorf("whoever starts a channel's call takes %q", got)
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
	for _, r := range []*Refusal{ErrUnknownAction, ErrNoTarget, ErrThroughToken, ErrNotInCall, ErrNotAllowed, ErrAboveYou, ErrYourself, ErrGuestCohost, ErrTargetGone, ErrCallOver} {
		if r.Status < 400 || r.Status >= 500 || r.Code == "" || !strings.HasSuffix(r.Msg, ".") {
			t.Errorf("%+v", r)
		}
	}
}
