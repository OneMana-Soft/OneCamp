package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

func TestLevelsAreOrderedAndNamed(t *testing.T) {
	order := []Level{LevelNone, LevelView, LevelComment, LevelEdit, LevelFull}
	names := []string{"none", "view", "comment", "edit", "full"}
	for i, l := range order {
		if l.String() != names[i] {
			t.Errorf("%d is named %q, want %q", l, l.String(), names[i])
		}
		if i > 0 && !(order[i-1] < l) {
			t.Errorf("%s must rank above %s", l, order[i-1])
		}
		if got, ok := level.Named(names[i]); !ok || got != l {
			t.Errorf("%q reads as %s", names[i], got)
		}
	}
	if _, ok := level.Named("owner"); ok {
		t.Error("owner is not a level")
	}
	// The API says a level by name, both ways.
	raw, _ := json.Marshal(Decision{Level: LevelEdit, Was: LevelFull})
	var back struct {
		Level string `json:"level"`
		Was   Level  `json:"was"`
	}
	if err := json.Unmarshal(raw, &back); err != nil || back.Level != "edit" || back.Was != LevelFull {
		t.Errorf("a decision reads back as %s (%v)", raw, err)
	}
	var bad Level
	if err := json.Unmarshal([]byte(`"admin"`), &bad); err == nil {
		t.Error("a level that isn't one reads without an error")
	}
}

// What a change asks for, as business/Doc makes it: people and teams by uuid in
// lower case, a team at most edit, taken away at none, and general access.
func TestAChangeIsReadAsTheDocMakesIt(t *testing.T) {
	before := &docBusiness.Sharing{General: docBusiness.GeneralAccess{Private: false, Level: LevelComment}}
	edit, none := LevelEdit, LevelNone
	yes := true
	for _, c := range []struct {
		name    string
		change  Change
		general *docBusiness.GeneralAccess
	}{
		{"privacy alone keeps the level", Change{Private: &yes}, &docBusiness.GeneralAccess{Private: true, Level: LevelComment}},
		{"none makes it private", Change{General: &none}, &docBusiness.GeneralAccess{Private: true, Level: LevelView}},
		{"a level opens it to the workspace", Change{General: &edit}, &docBusiness.GeneralAccess{Private: false, Level: LevelEdit}},
		{"a level for the space only", Change{General: &edit, Private: &yes}, &docBusiness.GeneralAccess{Private: true, Level: LevelEdit}},
		{"no general access asked for", Change{}, nil},
	} {
		sc, err := sharingChangeOf(before, c.change)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (sc.General == nil) != (c.general == nil) || (sc.General != nil && *sc.General != *c.general) {
			t.Errorf("%s: general %+v, want %+v", c.name, sc.General, c.general)
		}
	}
	sc, err := sharingChangeOf(before, Change{
		Add: []Grant{
			{Kind: PrincipalUser, UUID: "AAAAAAAA-0000-0000-0000-000000000001", Level: LevelFull},
			{Kind: PrincipalTeam, UUID: "bbbbbbbb-0000-0000-0000-000000000002", Level: LevelFull},
		},
		Remove:  []Grant{{UUID: "cccccccc-0000-0000-0000-000000000003"}},
		Replace: true,
	})
	if err != nil || sc.People["aaaaaaaa-0000-0000-0000-000000000001"] != LevelFull ||
		sc.Teams["bbbbbbbb-0000-0000-0000-000000000002"] != LevelEdit ||
		sc.People["cccccccc-0000-0000-0000-000000000003"] != LevelNone || !sc.Replace {
		t.Errorf("read as %+v (%v)", sc, err)
	}
	if _, err := sharingChangeOf(before, Change{Add: []Grant{{Kind: PrincipalSpace, UUID: "dddddddd-0000-0000-0000-000000000004", Level: LevelView}}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("a grant to a space: %v (a space's members reach a doc through its own space)", err)
	}
	if _, err := sharingChangeOf(before, Change{Add: []Grant{{UUID: "not-a-uuid", Level: LevelView}}}); !errors.Is(err, ErrNoSuchPrincipal) {
		t.Errorf("a grant to no one: %v", err)
	}
}

// business/Doc's decision, as the resolver gives it.
func TestADecisionIsTheDocsRule(t *testing.T) {
	d := decisionOf(docBusiness.DocDecision{Level: LevelComment, Unlocked: LevelEdit, Was: LevelComment, Lock: level.LockHard,
		Reasons: []level.Reason{level.SharedEdit, level.HardLock}})
	if d.Level != LevelComment || d.Unlocked != LevelEdit || !d.Locked || d.Lock != "hard" || len(d.Reasons) != 2 || d.Reasons[1] != "hard_lock" {
		t.Errorf("%+v", d)
	}
}

type fakeRules struct {
	Resolver
	levels map[string]Level
}

func (f fakeRules) Resolve(_ context.Context, _ Subject, res Resource) (Decision, error) {
	l, ok := f.levels[res.UUID]
	if !ok {
		return Decision{}, ErrNotFound
	}
	return Decision{Level: l}, nil
}

// A resolver that can't decide in batches is asked one resource at a time,
// and a resource that isn't there is left out.
func TestResolveAllAsksOneAtATimeWithoutBatches(t *testing.T) {
	was := Rules
	defer func() { Rules = was }()
	Rules = fakeRules{levels: map[string]Level{"a": LevelEdit, "b": LevelNone}}
	got, err := ResolveAll(context.Background(), Subject{}, []Resource{{KindDoc, "a"}, {KindDoc, "b"}, {KindDoc, "gone"}})
	if err != nil || len(got) != 2 || got[Resource{KindDoc, "a"}].Level != LevelEdit {
		t.Errorf("got %v (%v)", got, err)
	}
	if _, ok := any(rules{}).(Batch); !ok {
		t.Error("the resolver decides in batches")
	}
}
