package business

import "testing"

func TestSenderName(t *testing.T) {
	cases := map[string]string{
		"Sarah Lee <sarah@example.com>": "Sarah Lee",
		"<bob@example.com>":             "bob@example.com",
		"alice@example.com":             "alice@example.com",
		`"Quoted Name" <q@example.com>`: "Quoted Name",
		"":                              "",
	}
	for in, want := range cases {
		if got := senderName(in); got != want {
			t.Errorf("senderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFriendlyEventTime(t *testing.T) {
	if got := friendlyEventTime("2026-06-10"); got != "All day" {
		t.Errorf("date-only event = %q, want All day", got)
	}
	if got := friendlyEventTime(""); got != "" {
		t.Errorf("empty start = %q, want empty", got)
	}
	// RFC3339 should not return the raw string (it gets formatted).
	if got := friendlyEventTime("2026-06-10T15:00:00Z"); got == "2026-06-10T15:00:00Z" {
		t.Errorf("RFC3339 event should be formatted, got raw %q", got)
	}
}
