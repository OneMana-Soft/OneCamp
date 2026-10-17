package business

import (
	"context"
	"errors"
	"testing"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// An invite adds people and teams at the inviter's level or below, and does
// nothing else: an editor can't escalate, take access away, or reach general
// access, subpages or a replacement.
func TestAnInviteOnlyAddsAtYourLevelOrBelow(t *testing.T) {
	person := func(l Level) Grant {
		return Grant{Kind: PrincipalUser, UUID: "11111111-1111-4111-8111-111111111111", Level: l}
	}
	team := func(l Level) Grant {
		return Grant{Kind: PrincipalTeam, UUID: "22222222-2222-4222-8222-222222222222", Level: l}
	}
	edit, none := LevelEdit, LevelNone
	yes := true
	cases := []struct {
		name string
		lvl  Level
		c    Change
		want error
	}{
		{"an editor invites someone to edit", LevelEdit, Change{Add: []Grant{person(LevelEdit)}}, nil},
		{"an editor invites someone to view", LevelEdit, Change{Add: []Grant{person(LevelView)}}, nil},
		{"an editor invites a team to comment, until a date", LevelEdit, Change{Add: []Grant{{Kind: PrincipalTeam, UUID: team(0).UUID, Level: LevelComment, Until: &time.Time{}}}}, nil},
		{"full access invites someone with full access", LevelFull, Change{Add: []Grant{person(LevelFull)}}, nil},
		{"an editor can't give full access", LevelEdit, Change{Add: []Grant{person(LevelFull)}}, ErrAboveYourLevel},
		{"a commenter can't invite", LevelComment, Change{Add: []Grant{person(LevelComment)}}, ErrForbidden},
		{"a viewer can't invite", LevelView, Change{Add: []Grant{person(LevelView)}}, ErrForbidden},
		{"an invite takes nobody away", LevelEdit, Change{Add: []Grant{person(LevelView)}, Remove: []Grant{team(0)}}, ErrNotAnInvite},
		{"an invite can't open the doc to everyone", LevelEdit, Change{Add: []Grant{person(LevelView)}, General: &edit}, ErrNotAnInvite},
		{"nor make it private", LevelEdit, Change{Add: []Grant{person(LevelView)}, General: &none}, ErrNotAnInvite},
		{"nor say who its general audience is", LevelEdit, Change{Add: []Grant{person(LevelView)}, Private: &yes}, ErrNotAnInvite},
		{"nor reach the pages under it", LevelEdit, Change{Add: []Grant{person(LevelView)}, ApplyToSubpages: true}, ErrNotAnInvite},
		{"nor replace its sharing", LevelEdit, Change{Add: []Grant{person(LevelView)}, Replace: true}, ErrNotAnInvite},
		{"an invite names someone", LevelEdit, Change{}, ErrNotAnInvite},
		{"an invite gives some access", LevelEdit, Change{Add: []Grant{person(LevelNone)}}, ErrBadLevel},
	}
	for _, tc := range cases {
		if got := inviteRefusal(Decision{Level: tc.lvl}, tc.c); !errors.Is(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// A hard lock holds an editor at comment, and a commenter can't invite.
	locked := Decision{Level: LevelComment, Unlocked: LevelEdit, Locked: true}
	if got := inviteRefusal(locked, Change{Add: []Grant{person(LevelView)}}); !errors.Is(got, ErrForbidden) {
		t.Errorf("a hard-locked editor invites: %v, want %v", got, ErrForbidden)
	}
}

// Who may invite is one rule, MayInvite, which the invite itself and every
// offer to share that only adds people ask: edit or more, now. A commenter
// or reader can't; a hard lock holds an editor at comment; a doc in the trash
// takes no invites, whatever its people had.
func TestMayInviteIsEditOrMore(t *testing.T) {
	for _, c := range []struct {
		name string
		d    Decision
		want bool
	}{
		{"full access", Decision{Level: LevelFull, Unlocked: LevelFull, Was: LevelFull}, true},
		{"an editor", Decision{Level: LevelEdit, Unlocked: LevelEdit, Was: LevelEdit}, true},
		{"a commenter", Decision{Level: LevelComment, Unlocked: LevelComment, Was: LevelComment}, false},
		{"a reader", Decision{Level: LevelView, Unlocked: LevelView, Was: LevelView}, false},
		{"someone with no access", Decision{}, false},
		{"a hard-locked editor", Decision{Level: LevelComment, Unlocked: LevelEdit, Was: LevelComment, Locked: true, Lock: "hard"}, false},
		{"a hard-locked full access", Decision{Level: LevelFull, Unlocked: LevelFull, Was: LevelFull, Locked: true, Lock: "hard"}, true},
		{"its owner, in the trash", Decision{Deleted: true, Owner: true, Was: LevelFull}, false},
	} {
		if got := MayInvite(c.d); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// An invite never names someone the doc names already: a person it is shared
// with (whose grant it would change), a team on it, its owner, or the inviter.
func TestAnInviteNamesNobodyTheDocNamesAlready(t *testing.T) {
	const owner, shared, inviter, stranger = "aaaaaaaa-0000-4000-8000-000000000001", "aaaaaaaa-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000003", "aaaaaaaa-0000-4000-8000-000000000004"
	const onTeam, newTeam = "bbbbbbbb-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	before := &docBusiness.Sharing{
		Owner:  &dgraphStruct.DgraphUser{Uuid: owner},
		People: map[string]Level{shared: LevelFull},
		Teams:  map[string]*docBusiness.TeamShare{onTeam: {UUID: onTeam, Level: LevelView}},
	}
	who := Subject{UserUUID: inviter}
	for id, want := range map[string]bool{owner: true, shared: true, inviter: true, stranger: false} {
		if got := namedAlready(before, who, Grant{Kind: PrincipalUser, UUID: id}); got != want {
			t.Errorf("person %s: %v, want %v", id, got, want)
		}
	}
	// Ids are compared as uuids are: case doesn't matter.
	if !namedAlready(before, who, Grant{Kind: PrincipalUser, UUID: "AAAAAAAA-0000-4000-8000-000000000002"}) {
		t.Error("an upper-case uuid of someone shared with reads as a stranger")
	}
	if !namedAlready(before, who, Grant{Kind: PrincipalTeam, UUID: onTeam}) || namedAlready(before, who, Grant{Kind: PrincipalTeam, UUID: newTeam}) {
		t.Error("teams: a team on the doc is named already, a new one isn't")
	}
}

// Invite asks the resolver first: no access, or a doc in the trash, is said
// as every other doc check says it.
func TestInviteAsksTheResolverFirst(t *testing.T) {
	defer func(r Resolver) { Rules = r }(Rules)
	res := Resource{Kind: KindDoc, UUID: "cccccccc-0000-4000-8000-000000000001"}
	add := Change{Resource: res, Add: []Grant{{Kind: PrincipalUser, UUID: "cccccccc-0000-4000-8000-000000000002", Level: LevelView}}}
	for _, tc := range []struct {
		d    Decision
		want error
	}{
		{Decision{Level: LevelNone}, ErrNotFound},
		{Decision{Deleted: true, Was: LevelEdit}, docBusiness.ErrDocDeleted},
		{Decision{Level: LevelComment}, ErrForbidden},
	} {
		Rules = fixedResolver{d: tc.d}
		if _, err := Invite(context.Background(), Subject{UserUUID: "x"}, add); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.d, err, tc.want)
		}
	}
	if _, err := Invite(context.Background(), Subject{}, Change{Resource: Resource{Kind: "board", UUID: res.UUID}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("a board: %v", err)
	}
}

// fixedResolver answers one decision for everything.
type fixedResolver struct {
	rules
	d Decision
}

func (f fixedResolver) Resolve(context.Context, Subject, Resource) (Decision, error) { return f.d, nil }
