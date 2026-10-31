package businness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	"github.com/google/uuid"
)

type touch struct {
	doc, editor string
	at          time.Time
}

// touchWorld is newFileWorld for a new version: the file's current version,
// a recorded version at a known time, and the touches and search writes it
// asks for, kept.
func touchWorld(t *testing.T, recordErr error, fileID uuid.UUID, at time.Time) *[]touch {
	t.Helper()
	newFileWorld(t, recordErr, nil)
	record := recordVersion
	pr, pc, pt, pi := recordVersion, currentFiles, touchDoc, indexVersion
	t.Cleanup(func() { recordVersion, currentFiles, touchDoc, indexVersion = pr, pc, pt, pi })
	recordVersion = func(ctx context.Context, v docFileModels.NewVersion) (docFileModels.Version, error) {
		got, err := record(ctx, v)
		got.N, got.CreatedAt, got.CreatedBy = 2, at, v.CreatedBy
		return got, err
	}
	currentFiles = func(context.Context, []uuid.UUID) (map[uuid.UUID]docFileModels.Current, error) {
		return map[uuid.UUID]docFileModels.Current{fileID: {Version: docFileModels.Version{DocUUID: fileID, N: 1, Name: "msa.pdf", AttachmentID: uuid.New()}, Versions: 1}}, nil
	}
	touched := &[]touch{}
	touchDoc = func(_ context.Context, doc, editor string, at time.Time) error {
		*touched = append(*touched, touch{doc, editor, at})
		return nil
	}
	indexVersion = func(context.Context, []string, string) {}
	return touched
}

// A new version is the file's last change, by who added it (docs F1,
// DOCS-LEAD-2's ruling): when and who are written together, at the version's
// own time and for its uploader, so the file's row says "Maya · 3h ago" of
// the person who uploaded it. A version that couldn't be recorded changes
// nothing.
func TestANewVersionIsTheFilesLastChangeByItsUploader(t *testing.T) {
	fileID := uuid.New()
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	touched := touchWorld(t, nil, fileID, at)
	by := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: uuid.NewString()}
	// Renamed, so the title stays and nothing else is written.
	file := &dgraphStruct.DgraphDoc{Uuid: fileID.String(), NodeKind: "file", Title: "Master agreement"}
	v, err := AddFileVersion(context.Background(), by, file, NewFile{Name: "msa-signed.pdf", Body: strings.NewReader("%PDF"), Size: 4}, "Signed")
	if err != nil {
		t.Fatal(err)
	}
	if want := (touch{fileID.String(), by.Uuid, at}); len(*touched) != 1 || (*touched)[0] != want {
		t.Errorf("the version's touch: %+v, want %+v", *touched, want)
	}
	if !v.CreatedAt.Equal(at) {
		t.Errorf("the version says another time: %v", v.CreatedAt)
	}

	refused := touchWorld(t, errors.New("the version wasn't recorded"), fileID, at)
	if _, err := AddFileVersion(context.Background(), by, file, NewFile{Name: "msa-signed.pdf", Body: strings.NewReader("%PDF"), Size: 4}, ""); err == nil {
		t.Fatal("a version that wasn't recorded was added")
	}
	if len(*refused) != 0 {
		t.Errorf("a version that wasn't recorded touched the file: %+v", *refused)
	}
}
