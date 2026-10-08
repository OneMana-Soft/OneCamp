package topo

import (
	"reflect"
	"testing"
)

func TestOrder(t *testing.T) {
	cases := []struct {
		name            string
		items           []string
		deps            map[string][]string
		ordered, looped []string
	}{
		{
			name:    "nothing depends on anything: the order given",
			items:   []string{"c", "a", "b"},
			ordered: []string{"c", "a", "b"},
		},
		{
			name:    "each after what it depends on",
			items:   []string{"total", "tax", "price"},
			deps:    map[string][]string{"total": {"tax", "price"}, "tax": {"price"}},
			ordered: []string{"price", "tax", "total"},
		},
		{
			name:    "a dependency named twice, or not among the items",
			items:   []string{"b", "a"},
			deps:    map[string][]string{"b": {"a", "a", "elsewhere"}},
			ordered: []string{"a", "b"},
		},
		{
			name:    "a loop, and what depends on it, come last",
			items:   []string{"x", "loop1", "after", "loop2", "free"},
			deps:    map[string][]string{"loop1": {"loop2"}, "loop2": {"loop1"}, "after": {"loop2", "x"}},
			ordered: []string{"x", "free"},
			looped:  []string{"loop1", "after", "loop2"},
		},
		{
			name:   "an item depending on itself",
			items:  []string{"me"},
			deps:   map[string][]string{"me": {"me"}},
			looped: []string{"me"},
		},
	}
	for _, c := range cases {
		ordered, looped := Order(c.items, func(s string) []string { return c.deps[s] })
		if len(ordered) == 0 {
			ordered = nil
		}
		if !reflect.DeepEqual(ordered, c.ordered) || !reflect.DeepEqual(looped, c.looped) {
			t.Errorf("%s: got %v and %v, want %v and %v", c.name, ordered, looped, c.ordered, c.looped)
		}
	}
}
