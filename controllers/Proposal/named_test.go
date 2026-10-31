package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Proposal"
	"github.com/google/uuid"
)

// The list names who suggested each change, in one read, and never shows an
// address in place of a name; the AI is named by the client, and a failed
// read leaves the names out without failing the list.
func TestTheListNamesWhoSuggestedEachChange(t *testing.T) {
	maya, sam, agent := uuid.New(), uuid.New(), uuid.New()
	saved := authorNames
	t.Cleanup(func() { authorNames = saved })
	reads := 0
	authorNames = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		reads++
		if len(ids) != 3 {
			t.Errorf("asked for %d names, want 3 (the AI's isn't asked)", len(ids))
		}
		return map[uuid.UUID]string{maya: "Maya Patel", sam: "sam@acme.test", agent: "Launch helper"}, nil
	}
	list := []business.Proposal{
		{ID: "1", AuthorKind: business.AuthorPerson, AuthorUUID: maya.String()},
		{ID: "2", AuthorKind: business.AuthorPerson, AuthorUUID: sam.String()},
		{ID: "3", AuthorKind: business.AuthorAgent, AuthorUUID: agent.String()},
		{ID: "4", AuthorKind: business.AuthorAI, AuthorUUID: maya.String()},
	}
	got := named(context.Background(), list)
	want := []string{"Maya Patel", "", "Launch helper", ""}
	for i, l := range got {
		if l.AuthorName != want[i] || l.ID != list[i].ID {
			t.Errorf("%s: %q, want %q", l.ID, l.AuthorName, want[i])
		}
	}
	if reads != 1 {
		t.Errorf("%d reads, want one for the list", reads)
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"author_name":"Maya Patel"`) || !strings.Contains(string(raw), `"id":"1"`) {
		t.Errorf("the proposal's own fields and its author's name, side by side: %s", raw)
	}
	raw, _ = json.Marshal(got[1])
	if strings.Contains(string(raw), "author_name") || strings.Contains(string(raw), "@acme") {
		t.Errorf("an address in place of a name: %s", raw)
	}

	authorNames = func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		return nil, errors.New("database down")
	}
	for _, l := range named(context.Background(), list) {
		if l.AuthorName != "" {
			t.Errorf("a failed read named %s %q", l.ID, l.AuthorName)
		}
	}
}
