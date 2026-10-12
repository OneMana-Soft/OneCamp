//go:build integration

package domain

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/dgraph-io/dgo/v230"
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

	if err := SetDocCollabState(ctx, uuid.NewString(), "c3RyYXk=", "stray"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a uuid no doc has: %v, want ErrNotFound", err)
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

// A deleted doc keeps no collaboration state, however the save and the delete
// fall: a save that reaches the doc once it is deleted is refused by its own
// condition, and one whose transaction read the doc live before the delete
// committed is aborted when it commits, since both write the doc's uuid. The
// state was written with neither, after a separate read, so a delete landing
// between the two left the state of later edits on the trashed doc.
//
// Run: go test -tags=integration ./domain/Doc/ -run TestADeletedDocKeepsNoCollaborationState -v
func TestADeletedDocKeepsNoCollaborationState(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	trashed, racing := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:trashed", "dgraph.type": "Doc", "doc_uuid": trashed, "doc_body": "<p>Old plan.</p>",
			"doc_deleted_at": time.Now().UTC().Format(time.RFC3339)},
		{"uid": "_:racing", "dgraph.type": "Doc", "doc_uuid": racing, "doc_body": "<p>New plan.</p>",
			"doc_deleted_at": "0001-01-01T00:00:00Z"},
	})
	stateOf := func(id string) string {
		t.Helper()
		doc, err := GetCollabDocByUUID(ctx, id)
		if err != nil || doc == nil {
			t.Fatalf("read back: %+v %v", doc, err)
		}
		return doc.YjsState
	}

	// Deleted before the save reaches it.
	if err := SetDocCollabState(ctx, trashed, "c3RhdGU=", "hash-of-the-body"); !errors.Is(err, ErrDocDeleted) {
		t.Errorf("saving the state of a deleted doc: %v, want ErrDocDeleted", err)
	}
	if s := stateOf(trashed); s != "" {
		t.Errorf("a deleted doc took the state %q", s)
	}

	// Deleted while the save is in flight: its transaction has found the doc
	// live and written the state, and commits after the delete.
	req, err := docUpdateRequest(collabStateUpdate(racing, "c3RhdGU=", "hash-of-the-body"), liveDocFilter)
	if err != nil {
		t.Fatal(err)
	}
	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()
	if _, err := txn.Do(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := MarkDocDeleted(ctx, racing, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := txn.Commit(ctx); !errors.Is(err, dgo.ErrAborted) {
		t.Errorf("committing the save after the delete: %v, want it aborted", err)
	}
	if s := stateOf(racing); s != "" {
		t.Errorf("a doc deleted during the save took the state %q", s)
	}
	// Run again, as an aborted save is, it finds the doc deleted.
	if err := SetDocCollabState(ctx, racing, "c3RhdGU=", "hash-of-the-body"); !errors.Is(err, ErrDocDeleted) {
		t.Errorf("saving the state again: %v, want ErrDocDeleted", err)
	}
}
