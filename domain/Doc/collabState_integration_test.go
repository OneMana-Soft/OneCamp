//go:build integration

package domain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The collaboration service's saved Yjs state goes in beside the doc's body
// and comes back with it, and saving one for a doc that doesn't exist makes
// nothing: a mutation naming an empty variable would make a new node.
//
// Run: go test -tags=integration ./domain/Doc/ -run TestADocKeepsItsCollaborationState -v
func TestADocKeepsItsCollaborationState(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	docID := uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": docID, "doc_title": "Launch plan", "doc_body": "<p>Ship on Thursday.</p>"},
	})

	if err := SetDocCollabState(ctx, docID, "c3RhdGU=", "hash-of-the-body"); err != nil {
		t.Fatal(err)
	}
	doc, err := GetCollabDocByUUID(ctx, docID)
	if err != nil || doc == nil {
		t.Fatalf("read back: %+v %v", doc, err)
	}
	if doc.Body != "<p>Ship on Thursday.</p>" || doc.YjsState != "c3RhdGU=" || doc.YjsBodyHash != "hash-of-the-body" {
		t.Fatalf("got body %q state %q hash %q", doc.Body, doc.YjsState, doc.YjsBodyHash)
	}

	if err := SetDocCollabState(ctx, uuid.NewString(), "c3RyYXk=", "stray"); err != nil {
		t.Fatal(err)
	}
	if err := SetDocCollabState(ctx, "not-a-uuid", "c3RyYXk=", "stray"); err != ErrNoDocToUpdate {
		t.Fatalf("a malformed uuid: %v", err)
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ q(func: has(doc_yjs_state)) { count(uid) } }`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Q []struct {
			Count int `json:"count"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Q) != 1 || res.Q[0].Count != 1 {
		t.Fatalf("nodes holding a saved state: %+v, want only the doc's", res.Q)
	}
}
