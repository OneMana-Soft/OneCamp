package helpers

import (
	"slices"
	"testing"
)

// A save keeps every stored option it doesn't list, and takes out only those
// it names as removed.
func TestKeptOptionsTakesOutOnlyWhatTheSaveRemoves(t *testing.T) {
	stored := []string{"todo", "doing", "done", "blocked"}
	id := func(o string) string { return o }
	listing := func(ids ...string) func(string) bool {
		return func(o string) bool { return slices.Contains(ids, o) }
	}
	for _, c := range []struct {
		name     string
		listed   func(string) bool
		removed  []string
		kept     []string
		takenOut []string
	}{
		{"a save from before two options were added keeps them", listing("todo", "doing"), nil, []string{"done", "blocked"}, nil},
		{"a save listing every option keeps nothing more", listing(stored...), nil, nil, nil},
		{"a save with no options keeps them all", listing(), nil, stored, nil},
		{"an option named as removed is taken out", listing("todo", "doing"), []string{"done"}, []string{"blocked"}, []string{"done"}},
		{"an option both listed and named as removed stays listed", listing("todo"), []string{"todo", "done"}, []string{"doing", "blocked"}, []string{"done"}},
		{"a removal naming no stored option takes nothing", listing(), []string{"gone"}, stored, nil},
	} {
		removed := map[string]bool{}
		for _, r := range c.removed {
			removed[r] = true
		}
		kept, out := KeptOptions(stored, id, c.listed, removed)
		if !slices.Equal(kept, c.kept) || !slices.Equal(out, c.takenOut) {
			t.Errorf("%s: kept %v, took out %v; want %v, %v", c.name, kept, out, c.kept, c.takenOut)
		}
	}
}
