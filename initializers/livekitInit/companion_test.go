package livekitInit

import (
	"slices"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
)

// meet-04. A companion is another device of someone in a call: its identity
// is theirs and a tag the server drew, so it never takes their place in the
// room (LiveKit lets one connection hold an identity), and it reads back as
// theirs. Nothing else reads as a companion: a member, a guest, the agent, a
// tag that isn't the server's shape, a companion of a guest.
func TestACompanionIsItsPersonsAndNothingElseIsOne(t *testing.T) {
	first, err := NewCompanionIdentity("0x1f")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCompanionIdentity("0x1f")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == "0x1f" {
		t.Fatalf("two companions of 0x1f are %q and %q: each device needs an identity of its own", first, second)
	}
	for _, c := range []string{first, second} {
		if person, ok := CompanionOf(c); !ok || person != "0x1f" {
			t.Errorf("CompanionOf(%q) = %q, %v; want 0x1f's", c, person, ok)
		}
		if PersonOf(c) != "0x1f" {
			t.Errorf("PersonOf(%q) = %q, want 0x1f", c, PersonOf(c))
		}
	}
	for _, who := range []string{"guest-abc", "", " ", transcriberAgentIdentity, "0x1f~c~"} {
		if c, err := NewCompanionIdentity(who); err == nil {
			t.Errorf("a companion of %q (%q): only a member's graph uid has companions", who, c)
		}
	}
	tag := strings.TrimPrefix(first, "0x1f"+companionMark)
	for _, notOne := range []string{
		"0x1f", "guest-abc-1", transcriberAgentIdentity, "",
		"0x1f" + companionMark,                             // no tag
		"0x1f" + companionMark + "zz" + tag[2:],            // not the server's hex
		"0x1f" + companionMark + tag + "0",                 // too long
		"guest-abc" + companionMark + tag,                  // a guest's
		"0x1f" + companionMark + tag + companionMark + tag, // a companion's companion
		"0x" + companionMark + tag,                         // not a uid
	} {
		if person, ok := CompanionOf(notOne); ok {
			t.Errorf("CompanionOf(%q) = %q: not a companion", notOne, person)
		}
		if PersonOf(notOne) != notOne {
			t.Errorf("PersonOf(%q) = %q, want it unchanged", notOne, PersonOf(notOne))
		}
	}
}

// meet-04's risk line: two devices of one person must never both publish
// audio. A companion's token never lets it publish a microphone or a camera,
// whatever it is asked to grant, and never runs the room: it may share a
// screen (with its sound), and only what the call lets everyone share.
func TestACompanionsTokenNeverGrantsAMicrophoneOrACamera(t *testing.T) {
	s := testService()
	companion, err := NewCompanionIdentity("0x2")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.GenerateToken("room-1", companion, "Maya", true, "en", CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	c := claims(t, token)
	if c.Identity != companion || c.Video == nil || c.Video.Room != "room-1" {
		t.Fatalf("the companion's token: %+v", c)
	}
	if !slices.Equal(c.Video.CanPublishSources, CompanionSources) || !c.Video.GetCanPublish() {
		t.Errorf("a companion may publish %v (can publish %v), want only %v", c.Video.CanPublishSources, c.Video.GetCanPublish(), CompanionSources)
	}
	if c.Video.RoomAdmin {
		t.Error("a companion's token runs the room")
	}
	// Sharing held: nothing at all, which LiveKit reads only from CanPublish.
	held, err := s.GenerateToken("room-1", companion, "Maya", false, "", []string{"camera", "microphone"})
	if err != nil {
		t.Fatal(err)
	}
	if c := claims(t, held); c.Video.GetCanPublish() || len(c.Video.CanPublishSources) != 0 {
		t.Errorf("a companion while sharing is held may publish %v (%v), want nothing", c.Video.CanPublishSources, c.Video.GetCanPublish())
	}
	// Its person's own token is as it was.
	own, err := s.GenerateToken("room-1", "0x2", "Maya", false, "", CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	if got := claims(t, own).Video.CanPublishSources; !slices.Equal(got, CallPublishSources) {
		t.Errorf("Maya's own token may publish %v, want %v", got, CallPublishSources)
	}
	// What the call's server is told someone may publish later goes through
	// the same rule: a hold let go, a part given.
	if got := publishableBy(companion, CallPublishSources); !slices.Equal(got, CompanionSources) {
		t.Errorf("a companion given every source may publish %v, want %v", got, CompanionSources)
	}
	if got := publishableBy("0x2", []string{"microphone"}); !slices.Equal(got, []string{"microphone"}) {
		t.Errorf("Maya given her microphone may publish %v", got)
	}
}

// A companion is no person in a call: it never opens, keeps or ends one, is
// never counted among who's in it, and is never an attendee of its own.
func TestACompanionIsNoPersonInTheCall(t *testing.T) {
	companion, err := NewCompanionIdentity("0x3")
	if err != nil {
		t.Fatal(err)
	}
	if IsPerson(&livekit.ParticipantInfo{Identity: companion}) {
		t.Errorf("%q counts as a person in the call", companion)
	}
	if !IsPerson(&livekit.ParticipantInfo{Identity: "0x3"}) {
		t.Error("its person no longer counts")
	}
	if !IsCompanion(&livekit.ParticipantInfo{Identity: companion}) || IsCompanion(&livekit.ParticipantInfo{Identity: "0x3"}) || IsCompanion(nil) {
		t.Error("IsCompanion tells a companion from its person wrongly")
	}
}

// meet-04. A person's devices in a call are their companions, and their own
// connection when asked for: never someone else's, never a guest's, never an
// identity only shaped like a companion, and a companion's identity is
// nobody's person.
func TestAPersonsDevicesAreTheirOwnAndTheirCompanions(t *testing.T) {
	samPhone, err := NewCompanionIdentity("0x2b")
	if err != nil {
		t.Fatal(err)
	}
	samTab, err := NewCompanionIdentity("0x2b")
	if err != nil {
		t.Fatal(err)
	}
	mayaPhone, err := NewCompanionIdentity("0x2a")
	if err != nil {
		t.Fatal(err)
	}
	people := []*livekit.ParticipantInfo{
		{Identity: "0x2b"}, nil, {Identity: samPhone}, {Identity: "0x2a"}, {Identity: mayaPhone},
		{Identity: "guest-x-1"}, {Identity: "0x2b~c~nothex000000"}, {Identity: "0x2b~c~ABCDEF012345"}, {Identity: samTab},
	}
	ids := func(ps []*livekit.ParticipantInfo) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Identity)
		}
		return out
	}
	for _, c := range []struct {
		name   string
		person string
		own    bool
		want   []string
	}{
		{"Sam on every device", "0x2b", true, []string{"0x2b", samPhone, samTab}},
		{"Sam's companions only", "0x2b", false, []string{samPhone, samTab}},
		{"Maya on every device", "0x2a", true, []string{"0x2a", mayaPhone}},
		{"a guest has no companion", "guest-x-1", false, nil},
		{"a guest's own connection", "guest-x-1", true, []string{"guest-x-1"}},
		{"a companion is nobody's person", samPhone, true, nil},
		{"someone not here", "0x2c", true, nil},
		{"nobody", "", true, nil},
	} {
		if got := ids(DevicesOf(people, c.person, c.own)); !slices.Equal(got, c.want) {
			t.Errorf("%s: DevicesOf(%q, own=%v) = %v, want %v", c.name, c.person, c.own, got, c.want)
		}
	}
}
