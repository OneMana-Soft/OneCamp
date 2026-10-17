package position

import (
	"reflect"
	"testing"
	"time"
)

func ids(s []Sibling) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}

// Siblings sort by key; the ones made before keys existed come after, oldest
// first, and every reader breaks a tie the same way.
func TestOrderPutsKeyedSiblingsFirstThenTheOldestUnkeyed(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := []Sibling{
		{ID: "late", CreatedAt: t0.Add(2 * time.Hour)},
		{ID: "b", Position: "a1"},
		{ID: "early", CreatedAt: t0},
		{ID: "a", Position: "a0"},
		{ID: "bad", Position: "!!", CreatedAt: t0.Add(time.Hour)},
	}
	Order(s)
	if got, want := ids(s), []string{"a", "b", "early", "bad", "late"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestPlaceWritesOneKeyBetweenItsNeighbours(t *testing.T) {
	s := []Sibling{{ID: "a", Position: "a0"}, {ID: "b", Position: "a1"}, {ID: "c", Position: "a2"}}
	p, err := Place(s, "x", Where{After: "a"})
	if err != nil || p.Key <= "a0" || p.Key >= "a1" || len(p.Renumber) != 0 {
		t.Fatalf("after a: %+v %v", p, err)
	}
	if p, _ := Place(s, "x", Where{First: true}); p.Key >= "a0" || len(p.Renumber) != 0 {
		t.Fatalf("first: %+v", p)
	}
	if p, _ := Place(s, "x", Where{}); p.Key <= "a2" || len(p.Renumber) != 0 {
		t.Fatalf("last: %+v", p)
	}
	// Moving a sibling among its own: it's left out of the list it moves in.
	if p, _ := Place(s, "c", Where{After: "a"}); p.Key <= "a0" || p.Key >= "a1" {
		t.Fatalf("c after a: %+v", p)
	}
	if _, err := Place(s, "x", Where{After: "nobody"}); err != ErrNotSibling {
		t.Fatalf("after a stranger: %v", err)
	}
}

// Siblings with no keys yet (or the same key twice) are numbered afresh in the
// order they're shown, the moved one in its place, so nothing else moves.
func TestPlaceNumbersALevelAfreshWhenItsKeysCantOrderIt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := []Sibling{{ID: "a", CreatedAt: t0}, {ID: "b", CreatedAt: t0.Add(time.Minute)}, {ID: "c", CreatedAt: t0.Add(2 * time.Minute)}}
	p, err := Place(s, "x", Where{After: "b"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{p.Renumber["a"], p.Renumber["b"], p.Key, p.Renumber["c"]}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] == "" || keys[i] == "" || keys[i-1] >= keys[i] {
			t.Fatalf("keys %v don't give a, b, x, c", keys)
		}
	}
	dup := []Sibling{{ID: "a", Position: "a0"}, {ID: "b", Position: "a0"}}
	p, err = Place(dup, "x", Where{After: "a"})
	if err != nil || len(p.Renumber) == 0 {
		t.Fatalf("a repeated key wasn't renumbered: %+v %v", p, err)
	}
	// A keyed run followed by unkeyed siblings: adding at the end renumbers,
	// or the new key would sort before the unkeyed ones.
	mixed := []Sibling{{ID: "a", Position: "a0"}, {ID: "old", CreatedAt: t0}}
	p, _ = Place(mixed, "x", Where{})
	if p.Renumber["old"] == "" || p.Key <= p.Renumber["old"] {
		t.Fatalf("x went before an unkeyed sibling: %+v", p)
	}
}
