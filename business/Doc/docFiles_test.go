package businness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	"github.com/google/uuid"
)

// A row's file facts are its newest version (the F1a/F1b contract, 2:
// file {name, size_bytes, mime, versions, attachment_id}), read for a level's
// files in one statement, and for none when the level holds no file.
func TestARowsFileFactsAreItsNewestVersionReadOnceALevel(t *testing.T) {
	prev := currentFiles
	t.Cleanup(func() { currentFiles = prev })
	file, page := uuid.New(), uuid.New()
	att := uuid.New()
	var asked [][]uuid.UUID
	currentFiles = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]docFileModels.Current, error) {
		asked = append(asked, ids)
		return map[uuid.UUID]docFileModels.Current{file: {Version: docFileModels.Version{DocUUID: file, N: 3, AttachmentID: att,
			Name: "msa.pdf", SizeBytes: 48213, MIME: "application/pdf"}, Versions: 3}}, nil
	}
	docs := []*dgraphStruct.DgraphDoc{
		{Uuid: file.String(), NodeKind: "file"}, {Uuid: page.String()}, {Uuid: uuid.NewString(), NodeKind: "folder"},
	}
	facts, err := FileFactsOf(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || len(asked[0]) != 1 || asked[0][0] != file {
		t.Errorf("read the versions of %v; want the one file's, once", asked)
	}
	raw, _ := json.Marshal(facts[file.String()])
	want := `{"name":"msa.pdf","size_bytes":48213,"mime":"application/pdf","versions":3,"attachment_id":"` + att.String() + `"}`
	if string(raw) != want {
		t.Errorf("a file's facts: %s\nwant %s", raw, want)
	}
	asked = nil
	if facts, err := FileFactsOf(context.Background(), docs[1:]); err != nil || len(facts) != 0 || len(asked) != 0 {
		t.Errorf("a level with no file read %v: %v %v", asked, facts, err)
	}
}

// fileWorld swaps the stores under AddFile for recorders: what was stored,
// recorded, made, taken back.
type fileWorld struct {
	stored, removed []string
	recorded        []docFileModels.NewVersion
	dropped         []uuid.UUID
	made            []NewDoc
}

func newFileWorld(t *testing.T, recordErr, makeErr error) *fileWorld {
	t.Helper()
	w := &fileWorld{}
	ps, pr, prv, pd, pm := storeUpload, removeStored, recordVersion, dropUnseenDocs, makeFileNode
	t.Cleanup(func() { storeUpload, removeStored, recordVersion, dropUnseenDocs, makeFileNode = ps, pr, prv, pd, pm })
	storeUpload = func(_ context.Context, _ io.Reader, size int64, name string, uploader uuid.UUID, _ attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error) {
		key := uploader.String() + "/k_" + name
		w.stored = append(w.stored, key)
		return attachmentBusiness.Stored{ID: uuid.New(), ObjKey: key, SafeName: name, ContentType: "application/pdf", Size: 42}, nil
	}
	removeStored = func(_ context.Context, key string) error { w.removed = append(w.removed, key); return nil }
	recordVersion = func(_ context.Context, v docFileModels.NewVersion) (docFileModels.Version, error) {
		w.recorded = append(w.recorded, v)
		return docFileModels.Version{DocUUID: v.DocUUID, N: 1, AttachmentID: v.AttachmentID, Name: v.Name, SizeBytes: v.SizeBytes, MIME: v.MIME}, recordErr
	}
	dropUnseenDocs = func(_ context.Context, ids []uuid.UUID) (int64, error) {
		w.dropped = append(w.dropped, ids...)
		return 1, nil
	}
	makeFileNode = func(_ context.Context, _ *dgraphStruct.DgraphUser, in NewDoc, _ *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, error) {
		w.made = append(w.made, in)
		if makeErr != nil {
			return nil, makeErr
		}
		return &dgraphStruct.DgraphDoc{Uuid: in.UUID, Title: in.Title, NodeKind: in.NodeKind}, nil
	}
	return w
}

// A file is all or nothing (docs F1): its bytes are stored, recorded as its
// version 1, and then its node is made under that version's id. A version
// that can't be recorded takes the bytes back; a node that can't be made
// takes back the version and the bytes, and the error is the one that
// stopped it.
func TestAFileIsAllOrNothing(t *testing.T) {
	by := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}
	in := NewFile{Name: "  Q3 plan.pdf ", Body: strings.NewReader("%PDF-1.4"), Size: 8}

	w := newFileWorld(t, nil, nil)
	doc, facts, err := AddFile(context.Background(), by, in, nil)
	if err != nil || doc == nil || facts == nil {
		t.Fatalf("a file: %+v %+v %v", doc, facts, err)
	}
	if len(w.made) != 1 || w.made[0].NodeKind != "file" || w.made[0].Title != "Q3 plan.pdf" || w.made[0].UUID != w.recorded[0].DocUUID.String() {
		t.Errorf("the node made: %+v; recorded %+v", w.made, w.recorded)
	}
	if facts.Name != "Q3 plan.pdf" || facts.Versions != 1 || facts.SizeBytes != 42 || facts.MIME != "application/pdf" || len(w.removed)+len(w.dropped) != 0 {
		t.Errorf("facts %+v; taken back %v %v", facts, w.removed, w.dropped)
	}

	failed := errors.New("postgres down")
	w = newFileWorld(t, failed, nil)
	if _, _, err := AddFile(context.Background(), by, in, nil); !errors.Is(err, failed) {
		t.Errorf("a version that can't be recorded: %v", err)
	}
	if len(w.made) != 0 || len(w.removed) != 1 || w.removed[0] != w.stored[0] {
		t.Errorf("a version that can't be recorded made %v and took back %v of %v", w.made, w.removed, w.stored)
	}

	w = newFileWorld(t, nil, nil)
	if _, _, err := AddFile(context.Background(), by, in, &dgraphStruct.DgraphDoc{Uuid: "x", NodeKind: "file"}); !errors.Is(err, graph.ErrLeaf) {
		t.Errorf("a file under a file: %v", err)
	}
	if len(w.stored)+len(w.recorded)+len(w.made) != 0 {
		t.Errorf("a file under a file stored %v, recorded %v, made %v", w.stored, w.recorded, w.made)
	}

	leaf := errors.New("graph: that node can't hold others")
	w = newFileWorld(t, nil, leaf)
	if _, _, err := AddFile(context.Background(), by, in, nil); !errors.Is(err, leaf) {
		t.Errorf("a node that can't be made: %v", err)
	}
	if len(w.dropped) != 1 || w.dropped[0] != w.recorded[0].DocUUID || len(w.removed) != 1 {
		t.Errorf("a node that can't be made took back versions %v and bytes %v", w.dropped, w.removed)
	}
}

// A new version's name becomes the file's title, unless someone renamed the
// file: its title is no longer the name of the version it replaces.
func TestANewVersionsNameIsTheTitleUnlessTheFileWasRenamed(t *testing.T) {
	for _, c := range []struct {
		title, prev, next, want string
		changed                 bool
	}{
		{"msa.pdf", "msa.pdf", "msa-signed.pdf", "msa-signed.pdf", true},
		{"Master agreement", "msa.pdf", "msa-signed.pdf", "Master agreement", false},
		{"msa.pdf", "msa.pdf", "msa.pdf", "msa.pdf", false},
	} {
		if got, changed := titleAfterVersion(c.title, c.prev, c.next); got != c.want || changed != c.changed {
			t.Errorf("%+v: %q %v", c, got, changed)
		}
	}
	if _, err := versionNote(strings.Repeat("é", 501)); !errors.Is(err, ErrNoteTooLong) {
		t.Errorf("a note past 500 characters: %v", err)
	}
	if note, err := versionNote("  Signed by both  "); err != nil || note != "Signed by both" {
		t.Errorf("a note: %q %v", note, err)
	}
}
