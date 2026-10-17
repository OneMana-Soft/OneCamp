package models

import (
	"errors"
	"slices"
	"testing"
)

// A save keeps every option it doesn't list, after those it lists, and takes
// out only those it names as removed. A save from a page opened before
// "Blocked" was added (another tab) took Blocked, and every task's value of
// it, without a word.
func TestASaveKeepsTheOptionsItDoesntList(t *testing.T) {
	todo, doing, blocked := Option{ID: "a", Label: "Todo"}, Option{ID: "b", Label: "Doing"}, Option{ID: "c", Label: "Blocked"}
	stored := []Option{todo, doing, blocked}
	ids := func(opts []Option) []string {
		var out []string
		for _, o := range opts {
			out = append(out, o.ID)
		}
		return out
	}
	for _, c := range []struct {
		name     string
		sent     []Option
		removed  []string
		kept     []string
		takenOut []string
	}{
		{"a save from before Blocked was added keeps it", []Option{todo, doing}, nil, []string{"a", "b", "c"}, nil},
		{"a rename keeps the options it doesn't list", []Option{{ID: "b", Label: "In progress"}}, nil, []string{"b", "a", "c"}, nil},
		{"a save with no options keeps them all", nil, nil, []string{"a", "b", "c"}, nil},
		{"an option named as removed is taken out", []Option{todo, doing}, []string{"c"}, []string{"a", "b"}, []string{"c"}},
		{"a new option comes first, the ones not listed after", []Option{{ID: "d", Label: "Done"}}, []string{"b"}, []string{"d", "a", "c"}, []string{"b"}},
	} {
		got, out, err := mergeOptions(c.sent, stored, c.removed)
		if err != nil || !slices.Equal(ids(got), c.kept) || !slices.Equal(out, c.takenOut) {
			t.Errorf("%s: options %v, taken out %v, %v; want %v, %v", c.name, ids(got), out, err, c.kept, c.takenOut)
		}
	}

	// A save naming an option as one it doesn't list (whatever the case) can't
	// keep both: a value holds an option's id, so they can't be one.
	_, _, err := mergeOptions([]Option{{ID: "d", Label: "blocked"}}, stored, nil)
	var taken *OptionTakenError
	if !errors.As(err, &taken) || taken.Label != "Blocked" {
		t.Errorf("a new option named as a kept one: %v", err)
	}
	if _, _, err := mergeOptions([]Option{{ID: "d", Label: "Blocked"}}, stored, []string{"c"}); err != nil {
		t.Errorf("a new option named as one removed in the same save: %v", err)
	}
}
