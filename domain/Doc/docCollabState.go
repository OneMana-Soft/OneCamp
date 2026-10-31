package domain

// A save from the collaboration service, in one Dgraph upsert (docs programme
// A4: "one Dgraph upsert covers the body, snippet, state, hash and lineage").
//
// WHY ONE. The body and the Yjs state it was written from were two commits, and
// the second one's error was ignored: a failure left the old state beside the new
// body, the next open rebuilt the doc from HTML as a new copy, and a tab holding
// the old one doubled it. They are written together or not at all now.
//
// WHAT IT ANSWERS. The hash stored before, read by the same upsert, so the caller
// knows whether the body changed and runs the save's side effects only then
// (invariant I15): an edit undone, or a state-only save, fans out to nothing.
//
// WHAT IT WRITES. Only a live doc (liveDocFilter): one in the trash takes no
// saves (its editor may still be open) and answers ErrDocDeleted, as
// UpdateLiveDoc does, and a trash that commits while the save is in flight
// aborts it (collabSaveRequest). A body that hashes the same as the stored one is not
// written again; only the state and the lineage are, and updated_at stays, so a
// doc does not read "edited just now" for a save that changed nothing anyone can
// see. A conversion (a body the service changed as the doc loaded, with nobody's
// edit in it) is written, and updated_at stays too. A state-only save is stored
// only while the stored body is still the one it goes with.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// CollabWrite is one save from the collaboration service.
type CollabWrite struct {
	DocUUID string
	// Body and Snippet are the new body and its preview; ignored with StateOnly.
	Body    string
	Snippet string
	// State is the Yjs state (base64), Hash the hash of the body it goes with
	// (business/Doc collabBodyHash), Lineage the copy of the doc it is.
	State   string
	Hash    string
	Lineage string
	// StateOnly stores the state and lineage only, and only while the stored
	// body still hashes to Hash.
	StateOnly bool
	// EditorUUID is the person a changed body is credited to as the doc's last
	// editor (doc_updated_by); "" leaves it as it is. Ignored with StateOnly
	// and for a body that did not change.
	EditorUUID string
	// Conversion is a body with nobody's edit in it: what the collaboration
	// service changed as the doc loaded (block ids given, old content moved
	// over). It is stored like any body, but doc_updated_at stays: opening a
	// doc is not editing it (QA DSA-08: every seeded doc read "edited 1m ago"
	// once someone had opened it).
	Conversion bool
	At         time.Time
}

// CollabWritten is what a save found and did.
type CollabWritten struct {
	// PrevHash is the body hash stored before the save.
	PrevHash string
	// Stored is false for a state-only save whose body is not the stored one
	// any more, which wrote nothing.
	Stored bool
	// Hosted is a doc that belongs to something else (a table row's page,
	// D-18): a save of it runs only the hooks that keep it, never the ones
	// that put it where docs are found (business/Doc SaveHooks).
	Hosted bool
}

// collabUpsertQuery finds the doc, whether it is a live page, and whether its
// stored body is the one this save's state goes with; `found` comes back with
// the values as they were before the mutations. A folder, a file or a link
// has no body (pageOnly): nothing is written to one, and the save answers
// ErrNotAPage (collabSaveAnswer), the backstop behind the session's refusal. With an editor, `editor` is that
// person's node (collabEditorLine).
const collabUpsertQuery = `query q($id: string, $hash: string%s) {%s
	all as var(func: eq(doc_uuid, $id))
	live as var(func: uid(all)) @filter(` + liveDocFilter + ` AND ` + pageOnly + `)
	same as var(func: uid(live)) @filter(eq(doc_yjs_body_hash, $hash))
	found(func: uid(all)) {
		uid
		doc_yjs_body_hash
		doc_deleted_at
		doc_host_kind
		doc_node_kind
	}
}`

// collabEditorLine finds the last editor's node by their uuid.
var collabEditorLine = `
	editor as var(func: ` + dgraphquery.PersonByUUID("$editor") + `)`

// WriteCollabSave stores one save (see the top of this file).
func WriteCollabSave(ctx context.Context, w CollabWrite) (CollabWritten, error) {
	req, err := collabSaveRequest(w)
	if err != nil {
		return CollabWritten{}, err
	}
	resp, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WriteCollabSave doc %s err: %+v", w.DocUUID, err)
		return CollabWritten{}, err
	}
	return collabSaveAnswer(resp.GetJson(), w)
}

// collabSaveRequest is WriteCollabSave's upsert: the query that finds the doc,
// and the writes, each conditional on what it found. Every write sets the doc's
// uuid again, unchanged, as every other save and the trash do
// (docUpdateRequest, TrashDocs): the graph sees two transactions conflict only
// when both write the same thing, so a trash that commits while this save is in
// flight aborts it, and its retry finds the doc in the trash. Without the uuid
// the save committed after the trash, and its body or state landed on the
// trashed doc. Pure, so a test can hold a save open across a trash.
func collabSaveRequest(w CollabWrite) (*api.Request, error) {
	if _, err := uuid.Parse(w.DocUUID); err != nil {
		return nil, ErrNoDocToUpdate
	}
	at := w.At
	if at.IsZero() {
		at = time.Now()
	}
	var mutations []*api.Mutation
	add := func(set map[string]any, cond string) error {
		set["uid"] = "uid(live)"
		set["doc_uuid"] = w.DocUUID
		raw, err := json.Marshal(set)
		if err != nil {
			return err
		}
		mutations = append(mutations, &api.Mutation{SetJson: raw, Cond: cond})
		return nil
	}
	// The body is the stored one: the state and the lineage only.
	if err := add(map[string]any{"doc_yjs_state": w.State, "doc_yjs_lineage": w.Lineage}, "@if(eq(len(same), 1))"); err != nil {
		return nil, err
	}
	vars := map[string]string{"$id": w.DocUUID, "$hash": w.Hash}
	query := fmt.Sprintf(collabUpsertQuery, "", "")
	if !w.StateOnly {
		// A new body, on a live doc, with everything that goes with it; when
		// someone's edit is in it, the time it was edited too.
		body := map[string]any{
			"doc_body":          w.Body,
			"doc_snippet":       w.Snippet,
			"doc_yjs_state":     w.State,
			"doc_yjs_body_hash": w.Hash,
			"doc_yjs_lineage":   w.Lineage,
		}
		if !w.Conversion {
			body["doc_updated_at"] = at.UTC().Format(time.RFC3339Nano)
		}
		if err := add(body, "@if(eq(len(live), 1) AND eq(len(same), 0))"); err != nil {
			return nil, err
		}
		if _, err := uuid.Parse(w.EditorUUID); err == nil {
			// The last editor, for a body that changed, and only when that
			// person is in the graph: an edge to an empty variable would make
			// a node.
			vars["$editor"] = w.EditorUUID
			query = fmt.Sprintf(collabUpsertQuery, ", $editor: string", collabEditorLine)
			if err := add(map[string]any{"doc_updated_by": map[string]string{"uid": "uid(editor)"}},
				"@if(eq(len(live), 1) AND eq(len(same), 0) AND eq(len(editor), 1))"); err != nil {
				return nil, err
			}
		}
	}
	return &api.Request{Query: query, Vars: vars, Mutations: mutations}, nil
}

// collabSaveAnswer is what a save found and did, from its upsert's answer (the
// doc as it was before the writes). Pure.
func collabSaveAnswer(raw []byte, w CollabWrite) (CollabWritten, error) {
	var out struct {
		Found []struct {
			Hash      string     `json:"doc_yjs_body_hash"`
			DeletedAt *time.Time `json:"doc_deleted_at"`
			HostKind  string     `json:"doc_host_kind"`
			NodeKind  string     `json:"doc_node_kind"`
		} `json:"found"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CollabWritten{}, err
	}
	if len(out.Found) == 0 {
		return CollabWritten{}, dgraphModels.ErrNotFound
	}
	if helpers.IsSoftDeleted(out.Found[0].DeletedAt) {
		return CollabWritten{}, ErrDocDeleted
	}
	if out.Found[0].NodeKind != "" {
		return CollabWritten{}, ErrNotAPage
	}
	prev := out.Found[0].Hash
	return CollabWritten{PrevHash: prev, Stored: !w.StateOnly || prev == w.Hash, Hosted: out.Found[0].HostKind != ""}, nil
}
