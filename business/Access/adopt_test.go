package business

import (
	"maps"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A page under another takes its people, but its own owner, who has full by
// owning it; the parent's owner at full; and its teams: one rule for a new
// subpage (CopyOnCreate) and for a page adopting its new parent's sharing
// (AdoptChange).
func TestAPageTakesItsParentsPeopleButItsOwnOwner(t *testing.T) {
	p := &docBusiness.Sharing{
		Owner:  &dgraphStruct.DgraphUser{Uuid: alice},
		People: map[string]Level{bob: LevelEdit, cy: LevelView},
		Teams:  map[string]*docBusiness.TeamShare{"t": {UUID: "t", Level: LevelComment}},
	}
	people, teams := inheritedFrom(p, bob)
	if !maps.Equal(people, map[string]Level{cy: LevelView, alice: LevelFull}) || !maps.Equal(teams, map[string]Level{"t": LevelComment}) {
		t.Errorf("bob's page under alice's: %v %v", people, teams)
	}
	people, _ = inheritedFrom(p, alice)
	if !maps.Equal(people, map[string]Level{bob: LevelEdit, cy: LevelView}) {
		t.Errorf("alice's own page under hers: %v", people)
	}
	if people, teams = inheritedFrom(&docBusiness.Sharing{}, bob); len(people) != 0 || len(teams) != 0 {
		t.Errorf("a parent shared with nobody: %v %v", people, teams)
	}
}
