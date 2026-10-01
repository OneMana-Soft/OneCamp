package provider

import "testing"

func TestApplyStatusMap(t *testing.T) {
	defaults := map[string]string{
		"in progress": "inProgress",
		"done":        "done",
	}
	override := map[string]string{
		"in review": "inReview", // operator-confirmed for this import
	}

	cases := []struct {
		name   string
		source string
		want   string
	}{
		// Operator override wins.
		{"override beats default", "in review", "inReview"},
		// Default wins when no override.
		{"default applies", "in progress", "inProgress"},
		// Heuristic kicks in when neither map has it.
		{"heuristic to-do", "to do", "todo"},
		{"heuristic to-do hyphen", "to-do", "todo"},
		{"heuristic inprogress no space", "inprogress", "inProgress"},
		{"heuristic doing", "doing", "inProgress"},
		{"heuristic in-review", "in-review", "inReview"},
		{"heuristic resolved", "resolved", "done"},
		{"heuristic cancelled", "cancelled", "canceled"},
		{"heuristic wontfix maps to canceled", "wontfix", "canceled"},
		// Fallback when nothing matches.
		{"unknown falls to todo", "marshmallow", "todo"},
		// Empty input is safe.
		{"empty", "", "todo"},
		// Invalid override target falls through to default.
		{"invalid override falls through", "open", "todo"},
		// Case-insensitive.
		{"caps doesnt break", "DONE", "done"},
		// Whitespace trimmed.
		{"trim whitespace", "  done  ", "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyStatusMap(tc.source, override, defaults)
			if got != tc.want {
				t.Errorf("ApplyStatusMap(%q): got %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestApplyStatusMap_InvalidTargetIgnored(t *testing.T) {
	// An override value that isn't a OneCamp status must be ignored;
	// the function falls through to the default and then heuristic.
	override := map[string]string{"open": "fancyNotAStatus"}
	defaults := map[string]string{"open": "todo"}
	if got := ApplyStatusMap("open", override, defaults); got != "todo" {
		t.Fatalf("invalid override should fall through to default; got %q", got)
	}
}

func TestApplyPriorityMap(t *testing.T) {
	defaults := map[string]string{
		"highest": "high",
		"low":     "low",
	}

	cases := []struct {
		source, want string
	}{
		{"highest", "high"},
		{"low", "low"},
		// Heuristic
		{"urgent", "high"},
		{"critical", "high"},
		{"p0", "high"},
		{"p1", "high"},
		{"p2", "medium"},
		{"p3", "medium"},
		{"p4", "low"},
		{"p5", "low"},
		// Fallback
		{"weirdval", "medium"},
		{"", "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			if got := ApplyPriorityMap(tc.source, nil, defaults); got != tc.want {
				t.Errorf("ApplyPriorityMap(%q): got %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestStatusFromHeuristic_KnownSynonyms(t *testing.T) {
	cases := map[string]string{
		"open":          "todo",
		"to do":         "todo",
		"to-do":         "todo",
		"new":           "todo",
		"in progress":   "inProgress",
		"developing":    "inProgress",
		"backlog":       "backlog",
		"icebox":        "backlog",
		"in review":     "inReview",
		"qa":            "inReview",
		"done":          "done",
		"closed":        "done",
		"resolved":      "done",
		"completed":     "done",
		"canceled":      "canceled",
		"won't do":      "canceled",
		"abandoned":     "canceled",
		"duplicate":     "canceled",
		"truly-unknown": "",
	}
	for src, want := range cases {
		if got := StatusFromHeuristic(src); got != want {
			t.Errorf("StatusFromHeuristic(%q): got %q, want %q", src, got, want)
		}
	}
}
