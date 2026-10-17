package businness

import (
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A move that changes a page's space says into which and out of which, and
// names the page's owner when someone else moved it, so they are told; their
// own move, and a move that keeps the space, names nobody.
func TestASpaceMoveNamesTheOwnerToTell(t *testing.T) {
	at := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	x := Space{Kind: "project", UUID: "x"}
	e := movedEvent("d", "p", "b0b", &SpaceChange{To: x, Owner: "A11CE"}, at)
	if e.Kind != resourceEventBusiness.DocMoved || e.Data["parent"] != "p" || e.Data["space"] != "project:x" || e.Data["was"] != "none" ||
		!slices.Equal(e.Subjects, []string{"a11ce"}) || e.Actor.UserUUID != "b0b" || !e.At.Equal(at) {
		t.Errorf("someone else's move into a space: %+v", e)
	}
	own := movedEvent("d", "", "a11ce", &SpaceChange{From: Space{Kind: "team", UUID: "t"}, Owner: "A11CE"}, at)
	if own.Data["space"] != "none" || own.Data["was"] != "team:t" || len(own.Subjects) != 0 {
		t.Errorf("the owner's own move out of a space: %+v", own)
	}
	plain := movedEvent("d", "p", "b0b", nil, at)
	if len(plain.Data) != 1 || plain.Data["parent"] != "p" || len(plain.Subjects) != 0 {
		t.Errorf("a move within a space: %+v", plain)
	}
}

// The sharing a page would have in another space keeps its people, teams and
// general access, and takes the space's people: a private page shared with
// Bob alone, moved into a project Cy is in, opens to Cy.
func TestASharingInAnotherSpaceTakesItsPeople(t *testing.T) {
	before := &Sharing{
		DocUUID: "p", People: map[string]level.Level{"b0b": level.Edit}, Teams: map[string]*TeamShare{},
		General: GeneralAccess{Private: true, Level: level.View},
	}
	x := Space{Kind: "project", UUID: "x", Name: "Launch", Members: map[string]bool{"c0": true}}
	after := before.InSpace(x)
	if after.Space.Kind != "project" || after.Space.UUID != "x" || after.Space.Name != "Launch" || !after.Space.Members["c0"] {
		t.Fatalf("in Launch: %+v", after)
	}
	if after.LevelOf("c0") != level.View || before.LevelOf("c0") != level.None || after.LevelOf("b0b") != level.Edit {
		t.Errorf("cy %s (was %s), bob %s", after.LevelOf("c0"), before.LevelOf("c0"), after.LevelOf("b0b"))
	}
	x.Members["d0"] = true
	if after.Space.Members["d0"] {
		t.Error("the sharing holds on to the space's own list")
	}
	if out := after.InSpace(Space{}); out.Space.UUID != "" || out.LevelOf("c0") != level.None || (Space{}).Ref() != "none" {
		t.Errorf("out of any space: %+v", out)
	}
}

// A page moved to the top lands among its own section's top level; taken out
// of its space as well, among the top level it has without one: its owner's
// private pages, someone else's shared ones, or the workspace's.
func TestAPageLeavingItsSpaceLandsAmongTheTopItGoesTo(t *testing.T) {
	yes, no := true, false
	owner := &dgraphStruct.DgraphUser{Uid: "0x1"}
	inLaunch := func(private *bool) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{Uuid: "p", IsPrivate: private, Owner: owner, SpaceKind: "project", SpaceUUID: "x"}
	}
	for _, c := range []struct {
		name    string
		doc     *dgraphStruct.DgraphDoc
		viewer  string
		leaving bool
		want    string
	}{
		{"staying in its space", inLaunch(&yes), "0x1", false, "project:x"},
		{"its owner's private page, leaving", inLaunch(&yes), "0x1", true, "private"},
		{"someone else's private page, leaving", inLaunch(&yes), "0x2", true, "shared"},
		{"a workspace page, leaving", inLaunch(&no), "0x1", true, "workspace"},
	} {
		if got := topSectionFor(c.doc, c.viewer, c.leaving); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if d := inLaunch(&yes); topSectionFor(d, "0x1", true) != "private" || d.SpaceKind != "project" {
		t.Error("working out where it lands changed the doc read")
	}
}

// A space as a read finds it: who is in it by lower-case uuid, and no space
// at all without a kind and a uuid.
func TestASpaceIsWhoIsInIt(t *testing.T) {
	sp := spaceNamed("team", "t", "Design", []string{"A11CE", "b0b"})
	if sp.Kind != "team" || sp.UUID != "t" || sp.Name != "Design" || len(sp.Members) != 2 || !sp.Members["a11ce"] || !sp.Members["b0b"] {
		t.Errorf("the design team: %+v", sp)
	}
	for _, none := range []Space{spaceNamed("", "t", "Design", []string{"b0b"}), spaceNamed("team", "", "", nil)} {
		if none.Ref() != "none" || len(none.Members) != 0 {
			t.Errorf("no space: %+v", none)
		}
	}
}
