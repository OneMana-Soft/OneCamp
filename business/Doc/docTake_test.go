package businness

import (
	"fmt"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	"github.com/akashc777/OneCamp/business/Tree/position"
)

// P has H (hidden) and A (open), in that order; A has H2 (hidden) and A1
// (open, no key, so after H2); H has V, which the person can open, under it.
// Ids are their names.
func takeFixture() (treePage, []treePage) {
	root := treePage{UID: "0x1", UUID: "P"}
	return root, []treePage{
		{UID: "0x2", UUID: "A", ParentUUID: "P", Position: "a1"},
		{UID: "0x3", UUID: "H", ParentUUID: "P", Position: "a0"},
		{UID: "0x4", UUID: "A1", ParentUUID: "A"},
		{UID: "0x5", UUID: "H2", ParentUUID: "A", Position: "a5"},
		{UID: "0x6", UUID: "V", ParentUUID: "H"},
		{UID: "0x7", UUID: "Elsewhere", ParentUUID: "Q"}, // under no page here
	}
}

// A page the person may not act on stays with everything under it, a page
// they may open under it included: it isn't theirs to carry along. The pages
// that stay are its tops.
func TestATakeLeavesWhatThePersonMayNotActOnWithEverythingUnderIt(t *testing.T) {
	root, under := takeFixture()
	hidden := map[string]bool{"H": true, "H2": true}
	got := takeSubtree(root, under, func(p treePage) bool { return !hidden[p.UUID] })

	if fmt.Sprint(uuidsOf(got.Takes)) != "[P A A1]" {
		t.Fatalf("takes %v, want [P A A1]", uuidsOf(got.Takes))
	}
	if fmt.Sprint(uuidsOf(got.Stays)) != "[H H2]" {
		t.Fatalf("stays %v, want [H H2]", uuidsOf(got.Stays))
	}
	if fmt.Sprint(uuidsOf(got.With["H"])) != "[H V]" || fmt.Sprint(uuidsOf(got.With["H2"])) != "[H2]" {
		t.Fatalf("what stays with each top: %v", got.With)
	}
}

// The pages that stay are in the order the tree shows them, top to bottom: a
// page inside one higher up comes before a later sibling of that page, whatever
// their keys say, since keys order a level and never two levels against each
// other. Within a level, a page with no key comes after the keyed ones, oldest
// first (position.Order).
func TestThePagesThatStayAreInTheOrderTheTreeShowsThem(t *testing.T) {
	made := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	root := treePage{UID: "0x1", UUID: "P"}
	under := []treePage{
		{UID: "0x2", UUID: "A", ParentUUID: "P", Position: "a0"},
		{UID: "0x3", UUID: "H", ParentUUID: "P", Position: "a1"},
		{UID: "0x4", UUID: "H2", ParentUUID: "A", Position: "a5"},
		{UID: "0x5", UUID: "Late", ParentUUID: "P", CreatedAt: made.Add(time.Hour)},
		{UID: "0x6", UUID: "Early", ParentUUID: "P", CreatedAt: made},
	}
	got := takeSubtree(root, under, func(p treePage) bool { return p.UUID == "A" })
	if fmt.Sprint(uuidsOf(got.Stays)) != "[H2 H Early Late]" {
		t.Fatalf("stays %v, want [H2 H Early Late]: H2 sits inside A, which is above H", uuidsOf(got.Stays))
	}
}

// Someone who may act on every page takes the whole subtree, and nothing
// stays.
func TestATakeOfEverythingTakesTheWholeSubtree(t *testing.T) {
	root, under := takeFixture()
	got := takeSubtree(root, under, func(treePage) bool { return true })
	if fmt.Sprint(uuidsOf(got.Takes)) != "[P H V A H2 A1]" || len(got.Stays) != 0 || len(got.With) != 0 {
		t.Fatalf("takes %v, stays %v", uuidsOf(got.Takes), uuidsOf(got.Stays))
	}
	// Every page comes after the page it sits under.
	seen := map[string]bool{}
	for _, p := range got.Takes {
		if p.ParentUUID != "" && !seen[p.ParentUUID] {
			t.Fatalf("%s came before %s, the page it sits under", p.UUID, p.ParentUUID)
		}
		seen[p.UUID] = true
	}
}

// The root goes whatever may says of it: the caller decided that.
func TestATakeAlwaysTakesItsRoot(t *testing.T) {
	root, under := takeFixture()
	got := takeSubtree(root, under, func(treePage) bool { return false })
	if fmt.Sprint(uuidsOf(got.Takes)) != "[P]" || fmt.Sprint(uuidsOf(got.Stays)) != "[H A]" {
		t.Fatalf("takes %v, stays %v", uuidsOf(got.Takes), uuidsOf(got.Stays))
	}
}

// A delete takes what the person can open now; a delete for good, what they
// had full access to before it was deleted. Neither counts an admin's say,
// which the decision doesn't carry: it covers the page an admin chose, never a
// page under it.
func TestWhatEachActionTakesOfThePagesUnderIt(t *testing.T) {
	for _, c := range []struct {
		name         string
		d            DocDecision
		del, forGood bool
	}{
		{"opens it now", DocDecision{Level: level.View, Was: level.View}, true, false},
		{"edits it now", DocDecision{Level: level.Edit, Was: level.Edit}, true, false},
		{"can't open it", DocDecision{}, false, false},
		{"had full access, in the trash", DocDecision{Deleted: true, Was: level.Full}, false, true},
		{"could edit, in the trash", DocDecision{Deleted: true, Was: level.Edit}, false, false},
	} {
		if got := takesOnDelete(c.d); got != c.del {
			t.Errorf("%s: a delete takes it = %v, want %v", c.name, got, c.del)
		}
		if got := takesOnPurge(c.d); got != c.forGood {
			t.Errorf("%s: a delete for good takes it = %v, want %v", c.name, got, c.forGood)
		}
	}
}

// The pages moving up take the deleted page's place: right after it and
// before the page after it. A level whose keys can't order them leaves each
// its own key.
func TestPagesMovingUpTakeTheDeletedPagesPlace(t *testing.T) {
	sibs := []position.Sibling{{ID: "X", Position: "a0"}, {ID: "P", Position: "a1"}, {ID: "Y", Position: "a2"}}
	keys, ok := keysAfter(sibs, "P", 2)
	if !ok || len(keys) != 2 || !(keys[0] > "a1" && keys[0] < keys[1] && keys[1] < "a2") {
		t.Fatalf("keys %v (%v), want two between a1 and a2", keys, ok)
	}
	if keys, ok := keysAfter(sibs[:2], "P", 1); !ok || keys[0] <= "a1" {
		t.Fatalf("last on its level: %v (%v)", keys, ok)
	}
	for name, lvl := range map[string][]position.Sibling{
		"no key":        {{ID: "P"}, {ID: "Y", Position: "a2"}},
		"out of order":  {{ID: "P", Position: "a3"}, {ID: "Y", Position: "a2"}},
		"not its level": {{ID: "X", Position: "a0"}},
	} {
		if _, ok := keysAfter(lvl, "P", 1); ok {
			t.Errorf("%s: keys made where the level can't order them", name)
		}
	}
}
