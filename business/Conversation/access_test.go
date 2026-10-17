package business

import (
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func ptr[T any](v T) *T { return &v }

var gone = ptr(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

// The chat kinds are registered with the Access Resolver: a second rule for
// any of them panics where it is registered.
func TestTheChatKindsAreRegistered(t *testing.T) {
	for _, kind := range []string{KindChannel, KindDM, KindGroup, KindThread, KindCall} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s isn't registered with the Access Resolver", kind)
				}
			}()
			accessBusiness.RegisterKind(kind, channelRules{})
		}()
	}
}

// A channel decides as business/Channel CanRead and business/Send MayPostIn
// do today, for the person it was read for.
func TestAChannelDecidesAsTodaysChecks(t *testing.T) {
	for _, c := range []struct {
		name     string
		ch       dgraphStruct.DgraphChannel
		want     accessBusiness.Level
		archived bool
	}{
		{"a private channel, not a member", dgraphStruct.DgraphChannel{IsPrivate: ptr(true)}, accessBusiness.LevelNone, false},
		{"a channel whose privacy didn't load", dgraphStruct.DgraphChannel{}, accessBusiness.LevelNone, false},
		{"a public channel, not a member", dgraphStruct.DgraphChannel{IsPrivate: ptr(false)}, LevelRead, false},
		{"a member", dgraphStruct.DgraphChannel{IsPrivate: ptr(true), IsMember: 1}, LevelPost, false},
		{"a member of an announcement channel", dgraphStruct.DgraphChannel{IsPrivate: ptr(false), IsMember: 1, PostPolicy: "admins_only"}, LevelReply, false},
		{"its moderator", dgraphStruct.DgraphChannel{IsPrivate: ptr(true), IsMember: 1, IsAdmin: 1, PostPolicy: "admins_only"}, LevelModerate, false},
		{"a moderator who left it", dgraphStruct.DgraphChannel{IsPrivate: ptr(true), IsAdmin: 1}, LevelRead, false},
		{"a member of an archived channel", dgraphStruct.DgraphChannel{IsPrivate: ptr(true), IsMember: 1, DeletedAt: gone}, LevelRead, true},
	} {
		c.ch.Uuid = "c"
		d := channelDecision(&c.ch)
		if d.Level != c.want || d.Archived != c.archived || d.Was < d.Level {
			t.Errorf("%s: got %s archived %v was %s, want %s archived %v", c.name, d.Level, d.Archived, d.Was, c.want, c.archived)
		}
	}
	if d := channelDecision(&dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: ptr(true), IsMember: 1, DeletedAt: gone}); d.Was != LevelPost {
		t.Errorf("an archived channel keeps what its member could do before: %s", d.Was)
	}
}

// A DM's people read it, and post while the other person can receive a
// message (business/Send MayMessage); a group's participants post in it.
func TestDMsAndGroupsDecideAsTodaysChecks(t *testing.T) {
	if d := dmDecision(&dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b"}); d.Level != LevelPost {
		t.Errorf("a DM with a person: %s", d.Level)
	}
	if d := dmDecision(&dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b", DeletedAt: gone}); d.Level != LevelRead {
		t.Errorf("a DM with someone who left: %s", d.Level)
	}
	if d := dmDecision(nil); d.Level != LevelRead {
		t.Errorf("a DM with nobody there: %s", d.Level)
	}
	if got := (Ref{KindDM, "a b"}).Other("a"); got != "b" {
		t.Errorf("the other person in a b: %q", got)
	}
	if got := (Ref{KindDM, "a a"}).Other("a"); got != "a" {
		t.Errorf("a DM with oneself: %q", got)
	}
	if got := (Ref{KindDM, "a b"}).Other("c"); got != "" {
		t.Errorf("someone not in the DM: %q", got)
	}
	if d := groupDecision(&dgraphStruct.DgraphDm{ParticipantIsMember: 1}); d.Level != LevelPost {
		t.Errorf("a group's participant: %s", d.Level)
	}
	if d := groupDecision(&dgraphStruct.DgraphDm{}); d.Level != accessBusiness.LevelNone {
		t.Errorf("someone outside a group: %s", d.Level)
	}
}

// A thread is its channel's members' (public or not), and a deleted root's
// thread, or an archived channel's, is read only.
func TestAThreadDecidesAsTheThreadRoutes(t *testing.T) {
	post := func(ch dgraphStruct.DgraphChannel, deleted *time.Time) *dgraphStruct.DgraphPost {
		return &dgraphStruct.DgraphPost{Channel: &ch, DeletedAt: deleted}
	}
	for _, c := range []struct {
		name          string
		p             *dgraphStruct.DgraphPost
		want          accessBusiness.Level
		deleted, arch bool
	}{
		{"a public channel's thread, not a member", post(dgraphStruct.DgraphChannel{IsPrivate: ptr(false)}, nil), accessBusiness.LevelNone, false, false},
		{"a member", post(dgraphStruct.DgraphChannel{IsMember: 1}, nil), LevelReply, false, false},
		{"a member of an announcement channel", post(dgraphStruct.DgraphChannel{IsMember: 1, PostPolicy: "admins_only"}, nil), LevelReply, false, false},
		{"a moderator", post(dgraphStruct.DgraphChannel{IsMember: 1, IsAdmin: 1}, nil), LevelModerate, false, false},
		{"a deleted root", post(dgraphStruct.DgraphChannel{IsMember: 1}, gone), LevelRead, true, false},
		{"an archived channel", post(dgraphStruct.DgraphChannel{IsMember: 1, DeletedAt: gone}, nil), LevelRead, false, true},
	} {
		d := postThreadDecision(c.p)
		if d.Level != c.want || d.Deleted != c.deleted || d.Archived != c.arch {
			t.Errorf("%s: got %s deleted %v archived %v", c.name, d.Level, d.Deleted, d.Archived)
		}
	}

	in := &dgraphStruct.DgraphDm{ParticipantIsMember: 1}
	if d := chatThreadDecision(&dgraphStruct.DgraphChat{DM: in}); d.Level != LevelReply {
		t.Errorf("a participant: %s", d.Level)
	}
	if d := chatThreadDecision(&dgraphStruct.DgraphChat{DM: &dgraphStruct.DgraphDm{}}); d.Level != accessBusiness.LevelNone {
		t.Errorf("someone outside the conversation: %s", d.Level)
	}
	if d := chatThreadDecision(&dgraphStruct.DgraphChat{DM: in, To: &dgraphStruct.DgraphUser{DeletedAt: gone}}); d.Level != LevelRead {
		t.Errorf("a message to someone who left: %s", d.Level)
	}
	if d := chatThreadDecision(&dgraphStruct.DgraphChat{DM: in, DeletedAt: gone}); d.Level != LevelRead || !d.Deleted || d.Was != LevelReply {
		t.Errorf("a deleted root: %+v", d)
	}
}

// A channel's call is its members': a public channel's other readers don't
// join it.
func TestAChannelCallIsItsMembers(t *testing.T) {
	if d := channelCallDecision(&dgraphStruct.DgraphChannel{IsPrivate: ptr(false)}); d.Level != accessBusiness.LevelNone {
		t.Errorf("a public channel's reader: %s", d.Level)
	}
	if d := channelCallDecision(&dgraphStruct.DgraphChannel{IsMember: 1}); d.Level != LevelPost {
		t.Errorf("a member: %s", d.Level)
	}
	if d := channelCallDecision(&dgraphStruct.DgraphChannel{IsMember: 1, IsAdmin: 1}); d.Level != LevelModerate {
		t.Errorf("a moderator: %s", d.Level)
	}
}
