package jira

import (
	"testing"
	"time"
)

// parseJiraTime must accept the multiple timestamp shapes Jira returns.
// We've seen all four formats in production payloads from different
// instance configurations; each test case covers one.
func TestParseJiraTime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		zero bool
	}{
		{"empty", "", true},
		{"atlassian classic 4-digit offset",
			"2025-05-24T10:11:22.987+0000", false},
		{"rfc3339nano with Z",
			"2025-05-24T10:11:22.987654321Z", false},
		{"rfc3339 with colon offset",
			"2025-05-24T10:11:22+05:30", false},
		{"4-digit offset no millis",
			"2025-05-24T10:11:22-0700", false},
		{"garbage", "not-a-date", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseJiraTime(tc.in)
			if tc.zero {
				if !got.IsZero() {
					t.Fatalf("expected zero time, got %v", got)
				}
				return
			}
			if got.IsZero() {
				t.Fatalf("expected non-zero time for %q", tc.in)
			}
			// Sanity: parsed time should be in 2025.
			if got.Year() != 2025 {
				t.Fatalf("expected year 2025, got %v from %q", got, tc.in)
			}
		})
	}
}

// projectKeyFromIssue prefers the explicit project ref but falls back
// to the issue key prefix (e.g. ENG-123 → ENG).
func TestProjectKeyFromIssue(t *testing.T) {
	t.Run("explicit project ref wins", func(t *testing.T) {
		iss := &jiraIssue{
			Key:    "ENG-123",
			Fields: jiraIssueFields{Project: &jiraProjectRef{Key: "PLATFORM"}},
		}
		if got := projectKeyFromIssue(iss); got != "PLATFORM" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to key prefix", func(t *testing.T) {
		iss := &jiraIssue{Key: "ENG-123"}
		if got := projectKeyFromIssue(iss); got != "ENG" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("empty key returns empty", func(t *testing.T) {
		iss := &jiraIssue{Key: ""}
		if got := projectKeyFromIssue(iss); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

// extractAtlassianText walks the ADF tree and returns concatenated text.
func TestExtractAtlassianText(t *testing.T) {
	adf := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "hello "},
					map[string]any{"type": "text", "text": "world"},
				},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "second"},
				},
			},
		},
	}
	got := extractAtlassianText(adf)
	want := "hello world\nsecond\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// isDoneStatus must match Jira's done-state synonyms.
func TestIsDoneStatus(t *testing.T) {
	for _, s := range []string{"Done", "closed", "RESOLVED", "complete", "fixed"} {
		if !isDoneStatus(s) {
			t.Fatalf("expected %q to be done", s)
		}
	}
	for _, s := range []string{"In Progress", "todo", "open", ""} {
		if isDoneStatus(s) {
			t.Fatalf("expected %q to NOT be done", s)
		}
	}
}

// time.Now() reference to ensure the time import is exercised even if
// future test additions remove the only usage above.
var _ = time.Now
