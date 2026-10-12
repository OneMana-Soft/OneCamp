package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The collaboration service's state of a doc is saved as a save from the
// editor is: only on a live doc, decided in the same transaction as the
// write, and written with the doc's uuid, as a delete is, so a delete that
// commits while the save is in flight aborts it. The state used to be written
// on whatever doc had the uuid, naming neither, after a separate read had
// found the doc live.
func TestTheCollabStateIsSavedOnlyOnALiveDoc(t *testing.T) {
	id := uuid.NewString()
	req, err := docUpdateRequest(collabStateUpdate(id, "c3RhdGU=", "hash-of-the-body"), liveDocFilter)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Query, "doc as var(func: eq(doc_uuid, $id)) @filter("+liveDocFilter+")") {
		t.Errorf("the save's doc is not only a live one:\n%s", req.Query)
	}
	if req.Vars["$id"] != id || len(req.Mutations) != 1 || req.Mutations[0].Cond != "@if(eq(len(doc), 1))" {
		t.Fatalf("the save is not conditional on that doc: vars %v, mutations %+v", req.Vars, req.Mutations)
	}
	var set map[string]any
	if err := json.Unmarshal(req.Mutations[0].SetJson, &set); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"uid": "uid(doc)", "doc_uuid": id, "doc_yjs_state": "c3RhdGU=", "doc_yjs_body_hash": "hash-of-the-body"}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("the save writes %v, want %v", set, want)
	}

	if _, err := docUpdateRequest(collabStateUpdate("not-a-uuid", "c3RhdGU=", "h"), liveDocFilter); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("a malformed uuid: %v, want ErrNoDocToUpdate", err)
	}
}
