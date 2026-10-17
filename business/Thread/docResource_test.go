package business

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A comment's files are passed to the graph write only as names, keys, kinds
// and sizes: a node id or an author sent by the client would otherwise be
// written as given, linking someone else's node to the comment.
func TestACommentsFilesCarryOnlyWhatTheClientMaySay(t *testing.T) {
	raw := json.RawMessage(`[
		{"uid": "0x99", "attachment_uuid": "a1", "attachment_file_name": "plan.pdf", "attachment_obj_key": "k/plan.pdf",
		 "attachment_type": "file", "attachment_size": 1200, "attachment_deleted_at": "2026-01-01T00:00:00Z",
		 "attachment_created_by": {"uid": "0x1"}, "attachment_task": {"uid": "0x2"}, "dgraph.type": ["Task"]},
		null,
		{"attachment_file_name": "no key"}
	]`)
	got, err := attachmentsOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("kept %d files, want 1 (a file with no stored key is no file)", len(got))
	}
	f := got[0]
	if f.Uid != "" || f.CreatedBy != nil || f.DeletedAt != nil || f.Task != nil || f.Project != nil || f.DType != nil {
		t.Errorf("a client-sent field reached the write: %+v", f)
	}
	if f.Uuid != "a1" || f.FileName != "plan.pdf" || f.ObjectKey != "k/plan.pdf" || f.Size != 1200 || f.Type != "file" {
		t.Errorf("the file itself was lost: %+v", f)
	}

	var inv *Invalid
	if _, err := attachmentsOf(json.RawMessage(`{"not": "a list"}`)); !errors.As(err, &inv) {
		t.Errorf("a malformed list: %v", err)
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"attachment_obj_key":"k"},`, maxAttachments+1), ",") + "]"
	if _, err := attachmentsOf(json.RawMessage(many)); !errors.As(err, &inv) {
		t.Errorf("%d files: %v", maxAttachments+1, err)
	}
	if got, err := attachmentsOf(nil); err != nil || got != nil {
		t.Errorf("no files: %v %v", got, err)
	}
}
