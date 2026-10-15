package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestIsImageAttachment(t *testing.T) {
	cases := []struct {
		name string
		att  *dgraphStruct.DgraphAttachment
		want bool
	}{
		{"nil", nil, false},
		{"raw mime image", &dgraphStruct.DgraphAttachment{RawType: "image/png"}, true},
		{"raw mime jpeg", &dgraphStruct.DgraphAttachment{RawType: "image/jpeg"}, true},
		{"coarse type image", &dgraphStruct.DgraphAttachment{Type: "image"}, true},
		{"pdf", &dgraphStruct.DgraphAttachment{RawType: "application/pdf", Type: "file"}, false},
		{"video", &dgraphStruct.DgraphAttachment{RawType: "video/mp4", Type: "video"}, false},
		{"empty", &dgraphStruct.DgraphAttachment{}, false},
	}
	for _, c := range cases {
		if got := isImageAttachment(c.att); got != c.want {
			t.Errorf("%s: isImageAttachment = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCollectImageRefs(t *testing.T) {
	if refs := CollectImageRefs(nil, nil); refs != nil {
		t.Fatalf("nil inputs should yield no refs, got %v", refs)
	}

	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC) // newer

	media := []*dgraphStruct.DgraphAttachment{
		{Uuid: "p1", ObjectKey: "u/1_shot.png", FileName: "shot.png", RawType: "image/png"},
		{Uuid: "p2", ObjectKey: "u/2_doc.pdf", FileName: "doc.pdf", RawType: "application/pdf"}, // skipped (not image)
		{ObjectKey: "u/3_shot.png", RawType: "image/png"},                                       // skipped (no id to read it through its place by)
	}
	comments := []*dgraphStruct.DgraphComment{
		{CreatedAt: &t1, Attachments: []*dgraphStruct.DgraphAttachment{
			{Uuid: "c_old", ObjectKey: "u/old.jpg", FileName: "old.jpg", Type: "image"},
		}},
		{CreatedAt: &t2, Attachments: []*dgraphStruct.DgraphAttachment{
			{Uuid: "c_new", ObjectKey: "u/new.jpg", FileName: "new.jpg", RawType: "image/jpeg"},
			{Uuid: "c_new", ObjectKey: "u/new.jpg", FileName: "new.jpg", RawType: "image/jpeg"}, // dup by uuid
		}},
		nil,
	}

	refs := CollectImageRefs(media, comments)
	// Expect 3 image refs (deduped, image-only), MOST-RECENT FIRST:
	// newest comment (c_new) → older comment (c_old) → root message (p1). They
	// carry no object key: the key a message lists is its sender's to write,
	// so PlaceImageRefs reads each one's from its upload.
	if len(refs) != 3 {
		t.Fatalf("expected 3 image refs, got %d: %+v", len(refs), refs)
	}
	wantOrder := []string{"c_new", "c_old", "p1"}
	for i, want := range wantOrder {
		if refs[i].ID != want || refs[i].ObjKey != "" {
			t.Errorf("ref[%d] = %+v, want id %q and no key (full: %+v)", i, refs[i], want, refs)
		}
	}
	if refs[0].FileName != "new.jpg" {
		t.Errorf("expected filename carried through, got %q", refs[0].FileName)
	}
}
