package models

import (
	"testing"
	"time"
)

// A request's file becomes a new node with the upload's own fields only: a
// uid, a nested node, an adder or a deletion time it carries are dropped.
func TestNewAttachmentsKeepOnlyTheUpload(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	gone := at.Add(-time.Hour)
	in := []*DgraphAttachment{
		{
			Uid: "0x5", Uuid: "9F405860-AF54-40EC-9FD5-32694E9FB0B5", FileName: "notes.pdf", ObjectKey: "ben/notes.pdf",
			Type: "document", RawType: "application/pdf", Width: 1, Size: 2, Hight: 3, Duration: 4,
			Project:   &DgraphProject{Uid: "0x9", Name: "pwned"},
			Task:      &DgraphTask{Uid: "0xa"},
			CreatedBy: &DgraphUser{Uid: "0xb"},
			DeletedAt: &gone, DType: []string{"Doc"},
		},
		nil,
		{Uuid: "not-a-uuid", FileName: "x"},
	}
	out := NewAttachments(in, "0x2", at)
	if len(out) != 2 {
		t.Fatalf("%d files, want 2 (the nil one dropped)", len(out))
	}
	f := out[0]
	if f.Uid != "" || f.Project != nil || f.Task != nil || f.DeletedAt != nil {
		t.Errorf("the file keeps a node the request named: %+v", f)
	}
	if f.CreatedBy == nil || f.CreatedBy.Uid != "0x2" || f.CreatedAt == nil || !f.CreatedAt.Equal(at) {
		t.Errorf("the adder or the time are not the server's: %+v", f)
	}
	if len(f.DType) != 1 || f.DType[0] != "Attachment" {
		t.Errorf("type %v, want [Attachment]", f.DType)
	}
	if f.Uuid != "9f405860-af54-40ec-9fd5-32694e9fb0b5" || f.FileName != "notes.pdf" || f.ObjectKey != "ben/notes.pdf" ||
		f.Type != "document" || f.RawType != "application/pdf" || f.Width != 1 || f.Size != 2 || f.Hight != 3 || f.Duration != 4 {
		t.Errorf("the upload's own fields changed: %+v", f)
	}
	if out[1].Uuid == "not-a-uuid" || len(out[1].Uuid) != 36 {
		t.Errorf("an id that isn't a uuid is kept: %q", out[1].Uuid)
	}
	if NewAttachments(nil, "0x2", at) != nil {
		t.Error("no files should stay nil, so omitempty leaves the predicate out")
	}
}
