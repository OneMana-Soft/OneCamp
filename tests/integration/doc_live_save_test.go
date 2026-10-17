//go:build integration
// +build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/google/uuid"
)

// A save from the collaboration service is one upsert: the body, its preview,
// the Yjs state, the body's hash and the lineage are stored together or not at
// all, the hash stored before comes back with it (so an unchanged body fans out
// to nothing), and a doc in the trash takes no save (docs programme A4).
func TestACollabSaveIsOneUpsertThatSaysWhatChanged(t *testing.T) {
	ctx := context.Background()
	dg := SetupDgraph(t)
	live, trashed := uuid.NewString(), uuid.NewString()
	earlier := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	sam, ana := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam, "user_name": "sam"},
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana, "user_name": "ana"},
		{"uid": "_:a", "dgraph.type": "Doc", "doc_uuid": live, "doc_title": "Launch", "doc_body": "<p>One</p>", "doc_yjs_body_hash": "h1", "doc_updated_at": earlier, "doc_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": "_:b", "dgraph.type": "Doc", "doc_uuid": trashed, "doc_title": "Gone", "doc_body": "<p>Gone</p>", "doc_deleted_at": earlier},
	})

	// A new body: everything is written, and the old hash comes back. The
	// person who made it is the doc's last editor.
	w, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGUy", Hash: "h2", Lineage: "L1", EditorUUID: sam})
	if err != nil || w.PrevHash != "h1" || !w.Stored {
		t.Fatalf("new body: %+v %v", w, err)
	}
	if by := lastEditorOf(t, live); by != sam {
		t.Fatalf("the last editor is %q, want Sam", by)
	}
	got := readDoc(t, live)
	if got["doc_body"] != "<p>Two</p>" || got["doc_snippet"] != "Two" || got["doc_yjs_state"] != "c3RhdGUy" || got["doc_yjs_body_hash"] != "h2" || got["doc_yjs_lineage"] != "L1" {
		t.Fatalf("stored %v", got)
	}
	stamped := got["doc_updated_at"]

	// The same body again with a newer state: the state and the lineage only,
	// and nobody becomes its last editor for a save that changed nothing.
	w, err = docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGUz", Hash: "h2", Lineage: "L1", EditorUUID: ana})
	if err != nil || w.PrevHash != "h2" || !w.Stored {
		t.Fatalf("same body: %+v %v", w, err)
	}
	if by := lastEditorOf(t, live); by != sam {
		t.Fatalf("an unchanged save made %q the last editor", by)
	}
	got = readDoc(t, live)
	if got["doc_yjs_state"] != "c3RhdGUz" || got["doc_updated_at"] != stamped {
		t.Fatalf("same body: state %v, updated %v (was %v)", got["doc_yjs_state"], got["doc_updated_at"], stamped)
	}

	// A changed body credited to someone the graph does not hold leaves the
	// last editor as it was, and makes no node.
	w, err = docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, Body: "<p>Two and a half</p>", Snippet: "Two and a half", State: "c3RhdGUy", Hash: "h25", Lineage: "L1", EditorUUID: uuid.NewString()})
	if err != nil || !w.Stored {
		t.Fatalf("an unknown editor: %+v %v", w, err)
	}
	if by := lastEditorOf(t, live); by != sam {
		t.Fatalf("after an unknown editor, the last editor is %q", by)
	}
	// Back to the body the state-only saves below go with.
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGUz", Hash: "h2", Lineage: "L1"}); err != nil {
		t.Fatal(err)
	}

	// A state-only save for a body that is not the stored one writes nothing.
	w, err = docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, StateOnly: true, State: "c3RhbGU=", Hash: "h-other", Lineage: "L9"})
	if err != nil || w.Stored {
		t.Fatalf("stale state-only: %+v %v", w, err)
	}
	if got = readDoc(t, live); got["doc_yjs_state"] != "c3RhdGUz" || got["doc_yjs_lineage"] != "L1" {
		t.Fatalf("a stale state was stored: %v", got)
	}
	// One for the stored body is.
	w, err = docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: live, StateOnly: true, State: "c3RhdGU0", Hash: "h2", Lineage: "L2"})
	if err != nil || !w.Stored {
		t.Fatalf("state-only: %+v %v", w, err)
	}
	if got = readDoc(t, live); got["doc_yjs_state"] != "c3RhdGU0" || got["doc_yjs_lineage"] != "L2" || got["doc_body"] != "<p>Two</p>" {
		t.Fatalf("state-only stored %v", got)
	}

	// The trash takes nothing, and no doc is made for an unknown uuid.
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: trashed, Body: "<p>Back</p>", State: "eA==", Hash: "hx"}); !errors.Is(err, docDomain.ErrDocDeleted) {
		t.Fatalf("trash: %v", err)
	}
	if got = readDoc(t, trashed); got["doc_body"] != "<p>Gone</p>" {
		t.Fatalf("a doc in the trash was written: %v", got)
	}
	nobody := uuid.NewString()
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: nobody, Body: "<p>x</p>", State: "eA==", Hash: "hx"}); !errors.Is(err, dgraphModels.ErrNotFound) {
		t.Fatalf("unknown doc: %v", err)
	}
	if got = readDoc(t, nobody); got != nil {
		t.Fatalf("a save made a doc: %v", got)
	}

	// What the collaboration service opens the doc from.
	open, err := docDomain.GetCollabDocByUUID(ctx, live)
	if err != nil || open.Body != "<p>Two</p>" || open.YjsState != "c3RhdGU0" || open.YjsBodyHash != "h2" {
		t.Fatalf("the collab read: %+v %v", open, err)
	}

	// What the AI edition's re-embed reads after a changed save (business/AI
	// docSaveEmbed.go): the title, beside who may see the doc.
	doc, err := docDomain.GetDocPermissions(ctx, live)
	if err != nil || doc.Title != "Launch" {
		t.Fatalf("index read: %+v %v", doc, err)
	}
}

// lastEditorOf is the user_uuid of a doc's doc_updated_by, and fails the test
// when the doc has more than one, or the edge leads to a node with no uuid (one
// made by an edge to an empty variable).
func lastEditorOf(t *testing.T, id string) string {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($id: string) { d(func: eq(doc_uuid, $id)) { doc_updated_by { uid user_uuid } } }`,
		map[string]string{"$id": id})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		D []struct {
			By json.RawMessage `json:"doc_updated_by"`
		} `json:"d"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.D) != 1 {
		t.Fatalf("read the last editor: %v %s", err, resp.Json)
	}
	if len(out.D[0].By) == 0 {
		return ""
	}
	var by struct {
		Uid  string `json:"uid"`
		Uuid string `json:"user_uuid"`
	}
	if err := json.Unmarshal(out.D[0].By, &by); err != nil {
		t.Fatalf("doc_updated_by is not one node: %s", out.D[0].By)
	}
	if by.Uuid == "" {
		t.Fatalf("doc_updated_by leads to a node with no uuid: %s", out.D[0].By)
	}
	return by.Uuid
}

func readDoc(t *testing.T, id string) map[string]any {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($id: string) { d(func: eq(doc_uuid, $id)) { doc_body doc_snippet doc_yjs_state doc_yjs_body_hash doc_yjs_lineage doc_updated_at } }`,
		map[string]string{"$id": id})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		D []map[string]any `json:"d"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.D) == 0 {
		return nil
	}
	return out.D[0]
}
