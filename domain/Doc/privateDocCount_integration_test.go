//go:build integration

package domain

import (
	"context"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The private-docs list counts what it lists. Its count was written
// count(~doc_created_by) @filter(...), the filter outside count(), and was
// suspected of counting every doc the person made (the Dgraph plan, 18 Oct
// 2026). On v25.3.3 it doesn't: Dgraph applies that filter to the edge it
// counts, as it would count(~doc_created_by @filter(...)). This holds it there
// across upgrades, against docs the list must leave out (a deleted one, a
// public one, one someone else now owns, one whose title doesn't match). With
// the filter taken off the count it reads 5 and this fails.
//
// Run: go test -tags=integration ./domain/Doc/ -run '^TestThePrivateDocListCountsWhatItLists$' -v
func TestThePrivateDocListCountsWhatItLists(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	zero := "0001-01-01T00:00:00Z"
	gone := time.Now().UTC().Format(time.RFC3339)
	doc := func(key, title string, private bool, deleted string, extra map[string]any) map[string]any {
		d := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": title,
			"doc_private": private, "doc_deleted_at": deleted, "doc_created_at": time.Now().UTC().Format(time.RFC3339),
			"doc_created_by": map[string]any{"uid": "_:me"}}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "me"},
		{"uid": "_:other", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "other"},
		doc("plan", "Launch plan", true, zero, nil),
		doc("notes", "Notes", true, zero, map[string]any{"doc_owner": map[string]any{"uid": "_:me"}}),
		doc("deleted", "Old plan", true, gone, nil),
		doc("public", "Public plan", false, zero, nil),
		doc("given", "Handed-over plan", true, zero, map[string]any{"doc_owner": map[string]any{"uid": "_:other"}}),
	})

	list, err := GetDgraphPrivateDocFromDgraph(ctx, uids["me"], 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Docs) != 2 {
		t.Fatalf("listed %d private docs, want 2 (Launch plan, Notes)", len(list.Docs))
	}
	if list.Count != 2 {
		t.Errorf("the private-docs list counts %d, lists 2", list.Count)
	}

	found, err := GetDgraphPrivateDocFromDgraphWithSearchText(ctx, uids["me"], &adapter.InputSearchDocName{SearchText: "plan", PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Docs) != 1 {
		t.Fatalf("found %d private docs titled plan, want 1 (Launch plan)", len(found.Docs))
	}
	if found.Count != 1 {
		t.Errorf("the private-docs search counts %d, finds 1", found.Count)
	}
}
