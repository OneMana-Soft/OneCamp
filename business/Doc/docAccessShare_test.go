package businness

import (
	"testing"

	"github.com/akashc777/OneCamp/business/Access/level"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A sharing as ReadSharing makes one: Olu owns it; Ann has full access, Bea
// may edit, Cy comment, Di view; the Design team (Eve, Fay) may comment; it is
// in project P's space (Gus, Bea), private, whose members may edit.
func sampleSharing() *Sharing {
	return &Sharing{
		DocUUID: "doc", Owner: &dgraphStruct.DgraphUser{Uuid: "olu"},
		People:  map[string]level.Level{"ann": level.Full, "bea": level.Edit, "cy": level.Comment, "di": level.View},
		Names:   map[string]*dgraphStruct.DgraphUser{},
		Teams:   map[string]*TeamShare{"design": {UUID: "design", Name: "Design", Level: level.Comment, Members: []string{"eve", "fay"}}},
		General: GeneralAccess{Private: true, Level: level.Edit},
		Space:   Space{Kind: "project", UUID: "p", Members: map[string]bool{"gus": true, "bea": true}},
	}
}

// The sharing's levels are the access rule's, read from the doc's side.
func TestASharingsLevelsAreTheRules(t *testing.T) {
	s := sampleSharing()
	for who, want := range map[string]level.Level{
		"olu": level.Full, "ann": level.Full, "bea": level.Edit, "cy": level.Comment, "di": level.View,
		"eve": level.Comment, "fay": level.Comment, "gus": level.Edit, "zed": level.None,
	} {
		if got := s.LevelOf(who); got != want {
			t.Errorf("%s: %s, want %s", who, got, want)
		}
	}
	if s.EveryoneLevel() != level.None {
		t.Errorf("a private doc is no one else's, got %s", s.EveryoneLevel())
	}
	// Made the workspace's, everyone gets its general level.
	open := s.With(SharingChange{General: &GeneralAccess{Private: false, Level: level.Comment}})
	if open.EveryoneLevel() != level.Comment || open.LevelOf("zed") != level.Comment || open.LevelOf("gus") != level.Comment {
		t.Errorf("open to the workspace at comment: everyone %s, zed %s, gus %s", open.EveryoneLevel(), open.LevelOf("zed"), open.LevelOf("gus"))
	}
	// A hard lock holds editors at comment; the owner and full access keep full.
	s.Lock = level.LockHard
	if s.LevelOf("bea") != level.Comment || s.LevelOf("gus") != level.Comment || s.LevelOf("ann") != level.Full || s.LevelOf("olu") != level.Full {
		t.Errorf("hard lock: bea %s gus %s ann %s olu %s", s.LevelOf("bea"), s.LevelOf("gus"), s.LevelOf("ann"), s.LevelOf("olu"))
	}
	// In the trash, nobody.
	s.Lock, s.Deleted = "", true
	if s.LevelOf("olu") != level.None || s.DecisionOf("olu").Was != level.Full {
		t.Error("a deleted doc is nobody's, and the trash decides by the level before")
	}
}

// A change says whose level it moves, and leaves the sharing it was made from
// alone.
func TestAChangeExplainsWhoseLevelMoves(t *testing.T) {
	s := sampleSharing()
	after := s.With(SharingChange{
		People: map[string]level.Level{"di": level.None, "zed": level.Edit, "OLU": level.View},
		Teams:  map[string]level.Level{"design": level.None},
	})
	got := map[string][2]level.Level{}
	for _, sh := range Explain(s, after).People {
		got[sh.UserUUID] = [2]level.Level{sh.From, sh.To}
	}
	want := map[string][2]level.Level{
		"di":  {level.View, level.None},
		"zed": {level.None, level.Edit},
		"eve": {level.Comment, level.None},
		"fay": {level.Comment, level.None},
	}
	if len(got) != len(want) {
		t.Fatalf("moved %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %v, want %v", k, got[k], v)
		}
	}
	if s.People["di"] != level.View || s.Teams["design"] == nil {
		t.Error("With changed the sharing it was made from")
	}
	if after.People["olu"] != level.None {
		t.Error("the owner is never given a grant: owning is full access")
	}

	// Replacing: exactly what is named, the owner still owning.
	repl := s.With(SharingChange{People: map[string]level.Level{"zed": level.View}, Replace: true})
	if len(repl.People) != 1 || repl.People["zed"] != level.View || len(repl.Teams) != 0 || repl.LevelOf("olu") != level.Full {
		t.Errorf("replace left %v and %d teams", repl.People, len(repl.Teams))
	}

	// Making it the workspace's moves everyone at once.
	ex := Explain(s, s.With(SharingChange{General: &GeneralAccess{Level: level.View}}))
	if ex.Everyone == nil || ex.Everyone.From != level.None || ex.Everyone.To != level.View {
		t.Errorf("everyone: %+v", ex.Everyone)
	}
}

// A team's grant is at most edit: full access is given to people.
func TestATeamIsGivenEditAtMost(t *testing.T) {
	s := sampleSharing()
	if got := s.With(SharingChange{Teams: map[string]level.Level{"design": level.Full}}).Teams["design"].Level; got != level.Edit {
		t.Errorf("a team given full has %s", got)
	}
	w := writeOf(SharingChange{Teams: map[string]level.Level{"design": level.Full}, People: map[string]level.Level{"x": level.None}})
	if w.Teams["design"] != "edit" || w.People["x"] != "" {
		t.Errorf("written as %v %v", w.Teams, w.People)
	}
}
