package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Route is today's one link rule: the channel path, and helpers.ChatPath for
// a DM or group.
func TestRouteIsTodaysLinkRule(t *testing.T) {
	const me, them, msg = "aaaa", "bbbb", "mmmm"
	for _, c := range []struct {
		ref  Ref
		msg  string
		want string
	}{
		{Ref{KindChannel, "cccc"}, "", "/app/channel/cccc"},
		{Ref{KindChannel, "cccc"}, msg, "/app/channel/cccc/mmmm"},
		{Ref{KindDM, me + " " + them}, msg, helpers.ChatPath(me+" "+them, me, msg)},
		{Ref{KindGroup, "gggg"}, "", helpers.ChatPath("gggg", me, "")},
		{Ref{KindThread, msg}, "", ""},
		{Ref{KindChannel, ""}, "", ""},
	} {
		if got := c.ref.Route(me, c.msg); got != c.want {
			t.Errorf("%+v %q: %q, want %q", c.ref, c.msg, got, c.want)
		}
	}
	if got := (Ref{KindDM, me + " " + them}).Route(me, ""); got != "/app/chat/"+them {
		t.Errorf("a DM opens at the other person: %q", got)
	}
}

// A conversation's resource is the Access Resolver's name for it.
func TestARefIsAResource(t *testing.T) {
	if r := (Ref{KindGroup, "g"}).Resource(); r.Kind != "group" || r.UUID != "g" {
		t.Errorf("%+v", r)
	}
}

// Pages reads nothing for nobody.
func TestPagesReadNothingForNobody(t *testing.T) {
	if _, err := Pages.Page(t.Context(), nil, Ref{KindChannel, "c"}, nil); err != ErrNotFound {
		t.Errorf("%v", err)
	}
}

// A grouping id names a DM when it holds two ids, else a group.
func TestChatRefTellsADMFromAGroup(t *testing.T) {
	if r := ChatRef("a b"); r != (Ref{KindDM, "a b"}) {
		t.Errorf("%+v", r)
	}
	if r := ChatRef("g"); r != (Ref{KindGroup, "g"}) {
		t.Errorf("%+v", r)
	}
}

// answering is a resolver that answers Resolve with answer and nothing else.
type answering struct {
	accessBusiness.Resolver
	answer func(accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error)
}

func (a answering) Resolve(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	return a.answer(who, res)
}

// Resolve asks the Access Resolver for the person and the conversation, and a
// conversation they can't read answers as one that isn't there.
func TestResolveAsksTheAccessResolver(t *testing.T) {
	was := accessBusiness.Rules
	defer func() { accessBusiness.Rules = was }()
	me := &userModels.UserInfo{}
	me.UserDgraphInfo.Uid, me.UserDgraphInfo.Uuid = "0x1", "aaaa"
	ref := Ref{KindChannel, "cccc"}
	broken := errors.New("the graph didn't answer")

	for _, c := range []struct {
		name    string
		level   accessBusiness.Level
		err     error
		wantErr error
	}{
		{"a member posts", LevelPost, nil, nil},
		{"a reader reads", LevelRead, nil, nil},
		{"someone who can't read", accessBusiness.LevelNone, nil, ErrNotFound},
		{"no such conversation", accessBusiness.LevelNone, accessBusiness.ErrNotFound, ErrNotFound},
		{"a kind nothing decides", accessBusiness.LevelNone, accessBusiness.ErrUnknownKind, ErrNotFound},
		{"a fault", accessBusiness.LevelNone, broken, broken},
	} {
		var asked accessBusiness.Subject
		var about accessBusiness.Resource
		accessBusiness.Rules = answering{answer: func(who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
			asked, about = who, res
			return accessBusiness.Decision{Level: c.level}, c.err
		}}
		d, err := Resolve(context.Background(), me, ref)
		if !errors.Is(err, c.wantErr) || (err == nil && d.Level != c.level) {
			t.Errorf("%s: %+v %v", c.name, d, err)
		}
		if asked.UserUID != "0x1" || asked.UserUUID != "aaaa" || about != (accessBusiness.Resource{Kind: "channel", UUID: "cccc"}) {
			t.Errorf("%s: asked for %+v about %+v", c.name, asked, about)
		}
	}
	if _, err := Resolve(context.Background(), nil, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("nobody: %v", err)
	}
}

// ResolveAll asks about many conversations at once and keeps those the person
// can read; one that isn't there, or that they can't read, is left out, and a
// fault fails the whole ask.
func TestResolveAllKeepsWhatThePersonCanRead(t *testing.T) {
	was := accessBusiness.Rules
	defer func() { accessBusiness.Rules = was }()
	me := &userModels.UserInfo{}
	me.UserDgraphInfo.Uid, me.UserDgraphInfo.Uuid = "0x1", "aaaa"
	read, closed, gone := Ref{KindChannel, "read"}, Ref{KindGroup, "closed"}, Ref{KindDM, "a b"}
	var broken error
	accessBusiness.Rules = answering{answer: func(_ accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
		switch {
		case broken != nil:
			return accessBusiness.Decision{}, broken
		case res == read.Resource():
			return accessBusiness.Decision{Level: LevelRead}, nil
		case res == closed.Resource():
			return accessBusiness.Decision{Level: accessBusiness.LevelNone}, nil
		}
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}}
	got, err := ResolveAll(context.Background(), me, []Ref{read, closed, gone})
	if err != nil || len(got) != 1 || got[read].Level != LevelRead {
		t.Fatalf("%v %v: want only the one they can read", got, err)
	}
	broken = errors.New("the graph didn't answer")
	if _, err := ResolveAll(context.Background(), me, []Ref{read}); !errors.Is(err, broken) {
		t.Errorf("a fault: %v", err)
	}
	if got, err := ResolveAll(context.Background(), nil, []Ref{read}); err != nil || len(got) != 0 {
		t.Errorf("nobody: %v %v", got, err)
	}
}
