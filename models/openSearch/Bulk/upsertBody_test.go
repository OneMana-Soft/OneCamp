package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// A bulk upsert writes each entry's fields onto it, makes the entries that
// aren't there, and retries a conflict as a single upsert does.
func TestUpsertBodyMakesOrUpdatesEachEntry(t *testing.T) {
	body, err := upsertBody("boards", []Upsert{
		{ID: "b1", Doc: map[string]any{"board_text": "Book the venue"}},
		{ID: "b2", Doc: json.RawMessage(`{"board_text":""}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(body, "\n")
	want := []string{
		`{"update":{"_id":"b1","_index":"boards","retry_on_conflict":5}}`,
		`{"doc":{"board_text":"Book the venue"},"doc_as_upsert":true}`,
		`{"update":{"_id":"b2","_index":"boards","retry_on_conflict":5}}`,
		`{"doc":{"board_text":""},"doc_as_upsert":true}`,
		``,
	}
	if len(lines) != len(want) {
		t.Fatalf("body %q", body)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %s, want %s", i, lines[i], want[i])
		}
	}
	if _, err := upsertBody("boards", []Upsert{{ID: "b3", Doc: func() {}}}); err == nil || !strings.Contains(err.Error(), "b3") {
		t.Fatalf("an entry that can't be written: %v", err)
	}
	if body, err := upsertBody("boards", nil); err != nil || body != "" {
		t.Fatalf("no entries: %q, %v", body, err)
	}
}
