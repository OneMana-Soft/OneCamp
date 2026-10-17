package business

import (
	"context"
	"errors"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	alice = "a11ce000-0000-4000-8000-000000000001"
	bob   = "b0b00000-0000-4000-8000-000000000002"
	cy    = "c0000000-0000-4000-8000-000000000003"
	dee   = "dee00000-0000-4000-8000-000000000004"
)

// Only a space's members and admins put pages into it, and a workspace admin
// acting themselves; nobody else, an admin's agent included.
func TestOnlyThePeopleInASpacePutPagesIntoIt(t *testing.T) {
	sp := docBusiness.Space{Kind: "project", UUID: "x", Members: map[string]bool{bob: true}}
	for _, c := range []struct {
		name string
		who  Subject
		want error
	}{
		{"a member", Subject{UserUUID: bob}, nil},
		{"a member, however their id is written", Subject{UserUUID: "B0B00000-0000-4000-8000-000000000002"}, nil},
		{"someone outside it", Subject{UserUUID: cy}, ErrNotInSpace},
		{"a workspace admin", Subject{UserUUID: cy, Admin: true}, nil},
		{"a workspace admin's agent", Subject{UserUUID: cy, Admin: true, AgentUUID: dee}, ErrNotInSpace},
	} {
		if got := mayEnter(c.who, sp); !errors.Is(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A move to another space is a change to the page's sharing: someone who may
// only edit the page, or can't open it, is refused before anything is read.
func TestASpaceMoveNeedsFullAccessToThePage(t *testing.T) {
	was := Rules
	defer func() { Rules = was }()
	Rules = fakeRules{levels: map[string]Level{"edit": LevelEdit, "view": LevelView}}
	ctx := context.Background()
	for id, want := range map[string]error{"edit": ErrSpaceNeedsFull, "view": ErrSpaceNeedsFull, "gone": ErrNotFound} {
		for _, kind := range []string{"project", ""} {
			if _, err := PrepareSpaceMove(ctx, Subject{UserUUID: bob, Admin: true}, Resource{KindDoc, id}, kind, "x"); !errors.Is(err, want) {
				t.Errorf("moving a page I %s into %q: %v, want %v", id, kind, err, want)
			}
		}
	}
}

// What a move into a space would do is counted by the space's people: the
// ones who can't open the page yet gain it, and out again they lose it.
func TestASpaceMoveIsCountedByTheSpacesPeople(t *testing.T) {
	ctx := context.Background()
	private := &docBusiness.Sharing{
		DocUUID: "p", Owner: &dgraphStruct.DgraphUser{Uuid: alice},
		People: map[string]Level{bob: LevelEdit}, Teams: map[string]*docBusiness.TeamShare{},
		General: docBusiness.GeneralAccess{Private: true, Level: LevelView},
	}
	x := docBusiness.Space{Kind: "project", UUID: "x", Name: "Launch", Members: map[string]bool{bob: true, cy: true, dee: true}}
	into := &SpaceMove{who: Subject{UserUUID: bob}, res: Resource{KindDoc, "p"}, to: x, before: private}
	ex, err := into.Explain(ctx, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	gained := map[string]bool{}
	for _, e := range ex.Effects {
		if e.From == LevelNone && e.To == LevelView {
			gained[e.UserUUID] = true
		} else {
			t.Errorf("%s goes from %s to %s", e.UserUUID, e.From, e.To)
		}
	}
	if len(gained) != 2 || !gained[cy] || !gained[dee] || ex.Pages != 3 || ex.Everyone != nil {
		t.Errorf("into Launch: %+v (cy and dee gain it; bob, who may edit it, keeps that)", ex)
	}
	out := &SpaceMove{who: Subject{UserUUID: alice}, res: Resource{KindDoc, "p"}, before: private.InSpace(x)}
	ex, err = out.Explain(ctx, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	lost := 0
	for _, e := range ex.Effects {
		if e.To == LevelNone && (e.UserUUID == cy || e.UserUUID == dee) {
			lost++
		}
	}
	if lost != 2 || len(ex.Effects) != 2 {
		t.Errorf("out of Launch: %+v (cy and dee lose it)", ex)
	}
}
