package business

import (
	"encoding/json"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

func entryJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A new comment on a doc is findable in global search from the moment it is
// written, by the people who may read the doc. It was written as deleted (-1),
// which global search leaves out until someone edits the comment, and with no
// readers, editors, commenters or maker for its doc (they were read from the
// mutation that adds the comment, which has none), so on a private doc even
// its owner couldn't find it. On a public doc it now says so, which a plain
// bool left out.
func TestANewDocCommentIsFindableByThoseWhoMayReadTheDoc(t *testing.T) {
	yes, no := true, false
	at := time.Now()
	by := &dgraphStruct.DgraphUser{Uuid: "author", UserName: "Priya"}
	private := &dgraphStruct.DgraphDoc{Uuid: "doc", Title: "Plans", IsPrivate: &yes,
		CreatedBy:      &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner"},
		ReadingUser:    []*dgraphStruct.DgraphUser{{Uuid: "reader"}},
		EditingUser:    []*dgraphStruct.DgraphUser{{Uuid: "owner"}, {Uuid: "editor"}},
		CommentingUser: []*dgraphStruct.DgraphUser{{Uuid: "author"}}}

	e := entryJSON(t, docCommentEntry(private, "c1", "looks good", by, at))
	if v, ok := e["deleted_date"]; ok && v != nil {
		t.Errorf("a new comment is written as deleted: deleted_date = %v", v)
	}
	if e["comment_doc_private"] != true || e["comment_doc_created_by_user_id"] != "owner" || e["comment_doc_title"] != "Plans" {
		t.Errorf("the comment's doc: private %v, made by %v, title %v", e["comment_doc_private"], e["comment_doc_created_by_user_id"], e["comment_doc_title"])
	}
	for list, want := range map[string]string{"comment_doc_reading_users": "reader", "comment_doc_commenting_users": "author"} {
		if got, _ := e[list].([]any); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want [%s]", list, e[list], want)
		}
	}
	if got, _ := e["comment_doc_editing_users"].([]any); len(got) != 2 {
		t.Errorf("comment_doc_editing_users = %v, want the owner and the editor", e["comment_doc_editing_users"])
	}
	if e["comment_body"] != "looks good" || e["comment_by_user_id"] != "author" || e["comment_by_user_full_name"] != "Priya" {
		t.Errorf("the comment itself: %v", e)
	}

	public := &dgraphStruct.DgraphDoc{Uuid: "doc2", Title: "Handbook", IsPrivate: &no}
	if v, ok := entryJSON(t, docCommentEntry(public, "c2", "nice", by, at))["comment_doc_private"]; !ok || v != false {
		t.Errorf("a comment on a public doc: comment_doc_private = %v (written: %v), want false written out", v, ok)
	}

	// Never on any other comment: the grant would open every one of them.
	if _, ok := entryJSON(t, &openSearchStruct.OpenSearchComment{Uuid: "t", CommentTaskUuid: "task"})["comment_doc_private"]; ok {
		t.Error("a task's comment carries comment_doc_private")
	}
	if _, ok := entryJSON(t, &openSearchStruct.OpenSearchAttachment{Uuid: "f", AttachmentChannelUuid: "ch"})["attachment_doc_private"]; ok {
		t.Error("a channel file carries attachment_doc_private")
	}
}
