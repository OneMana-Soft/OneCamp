package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckTaskView(t *testing.T) {
	state := json.RawMessage(`{"filters":[],"sort":[]}`)
	name, err := checkTaskView("mine", "  My   overdue ", state)
	if err != nil || name != "My overdue" {
		t.Fatalf("tidy: %q %v", name, err)
	}
	if _, err := checkTaskView("project:9f1c2a3e-1111-4222-8333-944455556666", "Bugs", state); err != nil {
		t.Fatalf("project scope: %v", err)
	}
	bad := []struct {
		scope, name string
		state       json.RawMessage
	}{
		{"everyone", "x", state},
		{"project:nope", "x", state},
		{"mine", "   ", state},
		{"mine", strings.Repeat("é", 61), state},
		{"mine", "x", json.RawMessage(`[1,2]`)},
		{"mine", "x", json.RawMessage(`{`)},
		{"mine", "x", json.RawMessage(`{"a":"` + strings.Repeat("a", 9000) + `"}`)},
	}
	for _, b := range bad {
		if _, err := checkTaskView(b.scope, b.name, b.state); err == nil {
			t.Errorf("accepted %q %q", b.scope, b.name)
		}
	}
}
