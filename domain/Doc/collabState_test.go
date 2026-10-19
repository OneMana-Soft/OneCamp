package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A save from the collaboration service is written only on a live doc, decided
// in the same transaction as the write, and every write in it sets the doc's
// uuid, as the trash does (TrashDocs), so a trash that commits while the save
// is in flight aborts it. A state saved after a separate read, naming no uuid,
// once landed on a doc that had gone to the trash in between.
func TestACollabSaveIsWrittenOnlyOnALiveDocWithItsUUID(t *testing.T) {
	id, editor := uuid.NewString(), uuid.NewString()
	for _, w := range []CollabWrite{
		{DocUUID: id, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGU=", Hash: "h2", Lineage: "L1", EditorUUID: editor},
		{DocUUID: id, StateOnly: true, State: "c3RhdGU=", Hash: "h2", Lineage: "L1"},
	} {
		req, err := collabSaveRequest(w)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(req.Query, "live as var(func: uid(all)) @filter("+liveDocFilter+")") {
			t.Errorf("the save's doc is not only a live one:\n%s", req.Query)
		}
		if req.Vars["$id"] != id || req.Vars["$hash"] != "h2" {
			t.Errorf("vars %v", req.Vars)
		}
		want := 3
		if w.StateOnly {
			want = 1
		}
		if len(req.Mutations) != want {
			t.Fatalf("state only %v: %d writes, want %d", w.StateOnly, len(req.Mutations), want)
		}
		for _, m := range req.Mutations {
			var set map[string]any
			if err := json.Unmarshal(m.SetJson, &set); err != nil {
				t.Fatal(err)
			}
			if set["uid"] != "uid(live)" || set["doc_uuid"] != id {
				t.Errorf("a write without the doc's uuid on the live doc: %v", set)
			}
			if !strings.Contains(m.Cond, "len(same)") {
				t.Errorf("a write not conditional on the stored body: %q", m.Cond)
			}
			if _, hasBody := set["doc_body"]; hasBody && !strings.Contains(m.Cond, "eq(len(live), 1)") {
				t.Errorf("a new body not conditional on a live doc: %q", m.Cond)
			}
		}
		if w.StateOnly && strings.Contains(string(req.Mutations[0].SetJson), "doc_body") {
			t.Error("a state-only save writes a body")
		}
	}

	if _, err := collabSaveRequest(CollabWrite{DocUUID: "not-a-uuid", State: "c3RhdGU="}); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("a malformed uuid: %v, want ErrNoDocToUpdate", err)
	}
}

// What the upsert found decides the answer: no doc, a doc in the trash, or the
// hash it replaced.
func TestACollabSaveSaysWhatItFound(t *testing.T) {
	w := CollabWrite{DocUUID: uuid.NewString(), StateOnly: true, Hash: "h2"}
	if _, err := collabSaveAnswer([]byte(`{"found":[]}`), w); !errors.Is(err, ErrNotFound) {
		t.Errorf("no doc: %v", err)
	}
	if _, err := collabSaveAnswer([]byte(`{"found":[{"doc_yjs_body_hash":"h2","doc_deleted_at":"2026-10-12T10:00:00Z"}]}`), w); !errors.Is(err, ErrDocDeleted) {
		t.Errorf("in the trash: %v", err)
	}
	got, err := collabSaveAnswer([]byte(`{"found":[{"doc_yjs_body_hash":"h1","doc_deleted_at":"0001-01-01T00:00:00Z"}]}`), w)
	if err != nil || got.PrevHash != "h1" || got.Stored {
		t.Errorf("a state for another body: %+v %v", got, err)
	}
	got, err = collabSaveAnswer([]byte(`{"found":[{"doc_yjs_body_hash":"h2","doc_deleted_at":"0001-01-01T00:00:00Z"}]}`), w)
	if err != nil || !got.Stored || got.Hosted {
		t.Errorf("a state for the stored body: %+v %v", got, err)
	}
	// A table row's page says so, so its save runs only the hooks that keep it.
	got, err = collabSaveAnswer([]byte(`{"found":[{"doc_yjs_body_hash":"h2","doc_deleted_at":"0001-01-01T00:00:00Z","doc_host_kind":"table_row"}]}`), w)
	if err != nil || !got.Hosted {
		t.Errorf("a hosted doc's save: %+v %v", got, err)
	}
	if !strings.Contains(collabUpsertQuery, "doc_host_kind") {
		t.Error("the save's read doesn't ask whether the doc is hosted")
	}
}

// Opening a doc is not editing it (QA DSA-08). A body the collaboration
// service changed as the doc loaded, with nobody's edit in it (block ids, old
// content moved over), is stored, but the doc's edited time stays; an edit
// moves it.
func TestAConversionKeepsTheEditedTime(t *testing.T) {
	id := uuid.NewString()
	bodyOf := func(w CollabWrite) map[string]any {
		t.Helper()
		req, err := collabSaveRequest(w)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range req.Mutations {
			var set map[string]any
			if err := json.Unmarshal(m.SetJson, &set); err != nil {
				t.Fatal(err)
			}
			if _, ok := set["doc_body"]; ok {
				return set
			}
		}
		t.Fatalf("no body written for %+v", w)
		return nil
	}
	edit := bodyOf(CollabWrite{DocUUID: id, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGU=", Hash: "h2", Lineage: "L1"})
	if _, ok := edit["doc_updated_at"]; !ok {
		t.Errorf("an edit keeps the old edited time: %v", edit)
	}
	conversion := bodyOf(CollabWrite{DocUUID: id, Body: "<p>Two</p>", Snippet: "Two", State: "c3RhdGU=", Hash: "h2", Lineage: "L1", Conversion: true})
	if _, ok := conversion["doc_updated_at"]; ok {
		t.Errorf("a conversion reads as an edit: %v", conversion)
	}
	if conversion["doc_body"] != "<p>Two</p>" || conversion["doc_yjs_body_hash"] != "h2" {
		t.Errorf("a conversion's body is not stored: %v", conversion)
	}
}
