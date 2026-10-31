package business

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// fileFetch is a provider whose download is fetch.
type fileFetch struct {
	importProvider.Provider
	fetch func(dest io.Writer) error
}

func (f fileFetch) FetchAttachment(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, _ importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	return "", 0, f.fetch(dest)
}

// writes is a download of n bytes, written a KB at a time as a provider's
// copy writes them, which then ends with end.
func writes(n int, end error) func(io.Writer) error {
	return func(w io.Writer) error {
		for left := n; left > 0; left -= 1024 {
			if _, err := w.Write(make([]byte, min(left, 1024))); err != nil {
				return err
			}
		}
		return end
	}
}

type upload = func(context.Context, io.Reader, int64, string, uuid.UUID, attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error)

// keeps is StoreUpload over storage that takes what it's given: the upload
// reads to the end and keeps it, refuses an empty one, and fails as its
// read fails.
func keeps(_ context.Context, r io.Reader, _ int64, name string, by uuid.UUID, _ attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return attachmentBusiness.Stored{}, fmt.Errorf("minio put: %w", err)
	}
	if len(b) == 0 {
		return attachmentBusiness.Stored{}, fmt.Errorf("upload validation: %w", uploadsafe.ErrEmptyBody)
	}
	id := uuid.New()
	return attachmentBusiness.Stored{ID: id, ObjKey: by.String() + "/" + id.String() + "_" + name, ContentType: "text/plain", Size: int64(len(b))}, nil
}

// The import keeps a file through StoreUpload (must-fix 2 of the tables
// landing review): the provider's download is the upload's reader, the cap
// stops the download, the scan's refusal says what it found, and a file
// that's stored but can't be recorded is taken back. Whatever happens, both
// ends of the pipe stop, so a worker never hangs on a file.
func TestAnImportKeepsAFileThroughTheOneUpload(t *testing.T) {
	const capBytes = 2048
	refused := errors.New("upload validation: refused")
	limited := &importProvider.ErrRateLimited{RetryAfter: time.Second}
	cases := []struct {
		name    string
		fetch   func(io.Writer) error
		upload  upload
		record  error // the attachments row's write
		want    storedFile
		wantErr func(error) bool
		removed bool
	}{
		{name: "a file is kept as its place's", fetch: writes(1500, nil), want: storedFile{Size: 1500, ContentType: "text/plain"}},
		{name: "a file of the cap is kept", fetch: writes(capBytes, nil), want: storedFile{Size: capBytes, ContentType: "text/plain"}},
		{name: "a file past the cap stops its download, and nothing is stored", fetch: writes(10*capBytes, nil), want: storedFile{Outcome: fileTooLarge}},
		{name: "a file the provider says is gone", fetch: writes(0, importProvider.ErrAttachmentGone), want: storedFile{Outcome: fileGone}},
		{name: "a file gone halfway", fetch: writes(1024, fmt.Errorf("read body: %w", importProvider.ErrAttachmentGone)), want: storedFile{Outcome: fileGone}},
		{name: "an empty file", fetch: writes(0, nil), want: storedFile{Outcome: fileEmpty}},
		{
			name:  "the scan's refusal names what it found",
			fetch: writes(1500, nil),
			upload: func(ctx context.Context, r io.Reader, size int64, name string, by uuid.UUID, o attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error) {
				if _, err := keeps(ctx, r, size, name, by, o); err != nil {
					return attachmentBusiness.Stored{}, err
				}
				return attachmentBusiness.Stored{}, fmt.Errorf("upload blocked: %w", &avscan.Infected{Signature: "Eicar-Test-Signature"})
			},
			want: storedFile{Outcome: fileInfected, Signature: "Eicar-Test-Signature"},
		},
		{
			name: "a rate limit is the provider's, to be waited out", fetch: writes(0, limited),
			wantErr: func(err error) bool { _, ok := importProvider.IsRateLimited(err); return ok },
		},
		{
			name:  "an upload refused at its start ends the download",
			fetch: writes(capBytes, nil),
			upload: func(_ context.Context, r io.Reader, _ int64, _ string, _ uuid.UUID, _ attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error) {
				_, _ = r.Read(make([]byte, 10))
				return attachmentBusiness.Stored{}, refused
			},
			wantErr: func(err error) bool { return errors.Is(err, refused) },
		},
		{
			name: "a file that can't be recorded is taken back", fetch: writes(1500, nil), record: errors.New("the database is away"),
			wantErr: func(err error) bool { return err != nil }, removed: true,
		},
	}
	by := uuid.New()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var recorded, removed []string
			wasUpload, wasCreate, wasRemove := storeUpload, createAttachment, removeStored
			t.Cleanup(func() { storeUpload, createAttachment, removeStored = wasUpload, wasCreate, wasRemove })
			storeUpload = keeps
			if c.upload != nil {
				storeUpload = c.upload
			}
			createAttachment = func(_ context.Context, id uuid.UUID, objKey string, owner uuid.UUID, srcKey, srcValue string) error {
				recorded = append(recorded, fmt.Sprint(id, " ", objKey, " ", owner, " ", srcKey, " ", srcValue))
				return c.record
			}
			removeStored = func(_ context.Context, objKey string) error {
				removed = append(removed, objKey)
				return nil
			}

			type result struct {
				got storedFile
				err error
			}
			done := make(chan result, 1)
			go func() {
				got, err := storeFile(context.Background(), fileFetch{fetch: c.fetch}, &importModels.Job{}, nil,
					importProvider.SourceAttachment{Name: "notes.txt"}, "task", "task-1", by, capBytes)
				done <- result{got, err}
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("storeFile never returned: the download or the upload is stuck on the pipe")
			}

			if c.wantErr != nil {
				if r.err == nil || !c.wantErr(r.err) {
					t.Fatalf("error %v, not the one wanted", r.err)
				}
			} else if r.err != nil {
				t.Fatalf("error %v", r.err)
			}
			got := r.got
			if got.Outcome != c.want.Outcome || got.Signature != c.want.Signature || got.Size != c.want.Size || got.ContentType != c.want.ContentType {
				t.Fatalf("kept %+v, want %+v", got, c.want)
			}
			kept := c.wantErr == nil && c.want.Outcome == ""
			if (kept || c.record != nil) != (len(recorded) == 1) || (kept && recorded[0] != fmt.Sprint(got.ID, " ", got.ObjKey, " ", by, " task task-1")) {
				t.Fatalf("attachments rows written: %q (kept %+v)", recorded, got)
			}
			if c.removed != (len(removed) == 1) {
				t.Fatalf("taken back: %q", removed)
			}
		})
	}
}
