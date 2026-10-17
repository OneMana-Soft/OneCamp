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
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": docID, "doc_title": "Launch plan", "doc_body": "<p>Ship on Thursday.</p>",
			"doc_yjs_body_hash": "hash-of-the-body", "doc_deleted_at": "0001-01-01T00:00:00Z"},
	})

	if _, err := WriteCollabSave(ctx, CollabWrite{DocUUID: docID, StateOnly: true, State: "c3RhdGU=", Hash: "hash-of-the-body"}); err != nil {
		t.Fatal(err)
	}
	doc, err := GetCollabDocByUUID(ctx, docID)
	if err != nil || doc == nil {
		t.Fatalf("read back: %+v %v", doc, err)
	}
	if doc.Body != "<p>Ship on Thursday.</p>" || doc.YjsState != "c3RhdGU=" || doc.YjsBodyHash != "hash-of-the-body" {
		t.Fatalf("got body %q state %q hash %q", doc.Body, doc.YjsState, doc.YjsBodyHash)
	}

	for _, w := range []CollabWrite{
		{DocUUID: uuid.NewString(), StateOnly: true, State: "c3RyYXk=", Hash: "stray"},
		{DocUUID: uuid.NewString(), Body: "<p>Stray.</p>", State: "c3RyYXk=", Hash: "stray"},
	} {
		if _, err := WriteCollabSave(ctx, w); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a uuid no doc has (state only %v): %v, want ErrNotFound", w.StateOnly, err)
		}
	}
	if _, err := WriteCollabSave(ctx, CollabWrite{DocUUID: "not-a-uuid", State: "c3RyYXk=", Hash: "stray"}); err != ErrNoDocToUpdate {
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

// A doc in the trash takes no save from the collaboration service, its body
// and its state alike, however the save and the trash fall: a save that
// reaches the doc once it is in the trash is refused by its own condition, and
// one whose transaction read the doc live before the trash committed is
// aborted when it commits, since both write the doc's uuid (collabSaveRequest,
// TrashDocs). A save that named no uuid committed after the trash, and the
// edits made after it landed on the trashed doc.
//
// Run: go test -tags=integration ./domain/Doc/ -run TestADeletedDocKeepsNoCollaborationState -v
func TestADeletedDocKeepsNoCollaborationState(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	trashed, racingState, racingBody := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:trashed", "dgraph.type": "Doc", "doc_uuid": trashed, "doc_body": "<p>Old plan.</p>", "doc_yjs_body_hash": "h-old",
			"doc_deleted_at": time.Now().UTC().Format(time.RFC3339)},
		{"uid": "_:state", "dgraph.type": "Doc", "doc_uuid": racingState, "doc_body": "<p>New plan.</p>", "doc_yjs_body_hash": "h-new",
			"doc_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": "_:body", "dgraph.type": "Doc", "doc_uuid": racingBody, "doc_body": "<p>New plan.</p>", "doc_yjs_body_hash": "h-new",
			"doc_deleted_at": "0001-01-01T00:00:00Z"},
	})
	read := func(id string) (body, state string) {
		t.Helper()
		doc, err := GetCollabDocByUUID(ctx, id)
		if err != nil || doc == nil {
			t.Fatalf("read back: %+v %v", doc, err)
		}
		return doc.Body, doc.YjsState
	}

	// In the trash before the save reaches it.
	for _, w := range []CollabWrite{
		{DocUUID: trashed, StateOnly: true, State: "c3RhdGU=", Hash: "h-old"},
		{DocUUID: trashed, Body: "<p>Edited after the delete.</p>", State: "c3RhdGU=", Hash: "h-edited"},
	} {
		if _, err := WriteCollabSave(ctx, w); !errors.Is(err, ErrDocDeleted) {
			t.Errorf("saving a doc in the trash (state only %v): %v, want ErrDocDeleted", w.StateOnly, err)
		}
	}
	if body, state := read(trashed); body != "<p>Old plan.</p>" || state != "" {
		t.Errorf("a doc in the trash took body %q, state %q", body, state)
	}

	// Trashed while the save is in flight: its transaction has found the doc
	// live and written to it, and commits after the trash.
	for _, c := range []struct {
		doc string
		w   CollabWrite
	}{
		{racingState, CollabWrite{DocUUID: racingState, StateOnly: true, State: "c3RhdGU=", Hash: "h-new"}},
		{racingBody, CollabWrite{DocUUID: racingBody, Body: "<p>Edited after the delete.</p>", State: "c3RhdGU=", Hash: "h-edited"}},
	} {
		req, err := collabSaveRequest(c.w)
		if err != nil {
			t.Fatal(err)
		}
		txn := dgraphInit.DgraphClient.NewTxn()
		if _, err := txn.Do(ctx, req); err != nil {
			t.Fatal(err)
		}
		// The doc goes to the trash the way beta deletes docs (docTree.go).
		if err := TrashDocs(ctx, c.doc, nil, "collab-save-race", "0x1", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := txn.Commit(ctx); !errors.Is(err, dgo.ErrAborted) {
			t.Errorf("committing the save (state only %v) after the trash: %v, want it aborted", c.w.StateOnly, err)
		}
		_ = txn.Discard(ctx)
		if body, state := read(c.doc); body != "<p>New plan.</p>" || state != "" {
			t.Errorf("a doc trashed during the save (state only %v) took body %q, state %q", c.w.StateOnly, body, state)
		}
		// Run again, as an aborted save is, it finds the doc in the trash.
		if _, err := WriteCollabSave(ctx, c.w); !errors.Is(err, ErrDocDeleted) {
			t.Errorf("saving again (state only %v): %v, want ErrDocDeleted", c.w.StateOnly, err)
		}
	}
}
