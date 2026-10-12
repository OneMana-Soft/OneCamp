package ai

import (
	"encoding/json"
	"reflect"
	"testing"
)

// An entry that exists is updated with only the fields a write sends. A write
// that knows who may see the content sends all of that rule's fields, empty
// ones too, so taking someone off a doc, a board or a task takes them off its
// entry; a write that doesn't know sends none and keeps what is stored.
func TestEntryJSONSendsTheWholeAccessRuleOrNone(t *testing.T) {
	fields := func(d EmbeddingDoc) map[string]any {
		t.Helper()
		raw, err := entryJSON(embeddingDocWithVector{EmbeddingDoc: d, Embedding: []float32{1, 0}})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["embedding"]; !ok {
			t.Fatal("the vector was dropped")
		}
		return m
	}
	empty := []any{}

	// A private doc shared with nobody but its maker.
	private := true
	doc := fields(EmbeddingDoc{ContentType: "doc", ContentUUID: "d", ContentText: "t", DocPrivate: &private,
		DocCreatedByUserID: "ana", DocEditingUsers: []string{"ana"}})
	for k, want := range map[string]any{
		"doc_private": true, "doc_created_by_user_id": "ana",
		"doc_reading_users": empty, "doc_editing_users": []any{"ana"}, "doc_commenting_users": empty,
	} {
		if got, ok := doc[k]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("doc entry %s = %v (sent %v), want %v", k, got, ok, want)
		}
	}

	// A comment's text edited: nothing about who may see it is sent.
	edited := fields(EmbeddingDoc{ContentType: "comment", ContentUUID: "c", ContentText: "t"})
	for _, k := range []string{"doc_private", "doc_reading_users", "doc_editing_users", "doc_commenting_users",
		"doc_created_by_user_id", "board_public", "board_reading_users", "task_assignee_user_id"} {
		if _, ok := edited[k]; ok {
			t.Errorf("an edited comment's write sends %s, which would overwrite what is stored", k)
		}
	}

	// A comment on a private board: board_public false is sent, not left out.
	board := fields(EmbeddingDoc{ContentType: "comment", ContentUUID: "c", ContentText: "t", BoardUUID: "b",
		BoardPublic: false, BoardCreatedByUserID: "ana"})
	for k, want := range map[string]any{
		"board_public": false, "board_created_by_user_id": "ana",
		"board_reading_users": empty, "board_editing_users": empty, "board_commenting_users": empty,
	} {
		if got, ok := board[k]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("board comment entry %s = %v (sent %v), want %v", k, got, ok, want)
		}
	}

	// An unassigned task says so.
	task := fields(EmbeddingDoc{ContentType: "task", ContentUUID: "t", ContentText: "t", ProjectUUID: "p"})
	if got, ok := task["task_assignee_user_id"]; !ok || got != "" {
		t.Errorf("task entry task_assignee_user_id = %v (sent %v), want \"\"", got, ok)
	}
	// A post's entry is as it always was.
	post := fields(EmbeddingDoc{ContentType: "post", ContentUUID: "p", ContentText: "t", ChannelUUID: "ch"})
	if _, ok := post["doc_private"]; ok {
		t.Error("a post's entry carries doc_private")
	}
}
