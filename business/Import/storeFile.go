package business

// storeFile is how an import keeps one file of the source's: fetched through
// the provider (so a provider's auth goes with it), kept by
// attachmentBusiness.StoreUpload (the one way an upload becomes a stored
// object: its type sniffed and made safe, a download forced, scanned as it
// goes), and written as an attachments row of its place. The attachments
// stage (a task's or a comment's file) and the tables stage (a files cell's)
// both keep files this way.

import (
	"context"
	"errors"
	"fmt"
	"io"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// storedFile is a file kept: its attachment's id and storage key, its size
// and type. Outcome says why it wasn't, when it wasn't.
type storedFile struct {
	ID          uuid.UUID
	ObjKey      string
	Size        int64
	ContentType string
	Outcome     string // "" kept; fileGone, fileEmpty, fileInfected or fileTooLarge
	Signature   string // what the scan found, for fileInfected
}

const (
	fileGone     = "gone"
	fileEmpty    = "empty"
	fileInfected = "infected"
	fileTooLarge = "too_large"
)

// What a file is kept through: seams for tests.
var (
	storeUpload      = attachmentBusiness.StoreUpload
	createAttachment = attachmentBusiness.CreateAttachment
	removeStored     = attachmentBusiness.RemoveStored
)

var (
	// errFetchStopped is a provider's download that ended without returning
	// (a panic, which GoSafeNamed recovers): the upload stops with it.
	errFetchStopped = errors.New("the provider's download stopped before it finished")
	// errUploadEnded is what a download still writing is told once the
	// upload has ended without it (the upload refused the file, or failed).
	errUploadEnded = errors.New("the upload ended before the download")
	// errOverLimit is a write past a cappedWriter's cap.
	errOverLimit = errors.New("the file is over the upload limit")
)

// storeFile keeps att for the person importing (by), as a file of the place
// srcKey/srcValue, at most maxBytes. A file the provider says is gone, an
// empty one, one the scan finds infected and one past maxBytes are not
// kept, and say so in Outcome; a rate limit and every other failure are
// errors, after which nothing is left stored.
func storeFile(ctx context.Context, prov importProvider.Provider, job *importModels.Job, opts importProvider.JobOptions,
	att importProvider.SourceAttachment, srcKey, srcValue string, by uuid.UUID, maxBytes int64) (storedFile, error) {
	// The provider's bytes stream through a pipe into the upload, capped as
	// they're written: a file past maxBytes fails its download, and the
	// upload with it, so none of it is stored.
	pr, pw := io.Pipe()
	capped := &cappedWriter{w: pw, left: maxBytes}
	var fetchErr error
	done := make(chan struct{})
	helpers.GoSafeNamed("business/Import/storeFile:storeFile", func() {
		defer close(done)
		// Always close (with the error, or plainly for none) so the upload's
		// read returns, whatever the provider's implementation does, a panic
		// in it too.
		fetchErr = errFetchStopped
		defer func() { _ = pw.CloseWithError(fetchErr) }()
		_, _, fetchErr = prov.FetchAttachment(ctx, job, opts, att, capped)
	})
	kept, err := storeUpload(ctx, pr, -1, att.Name, by, attachmentBusiness.StoreOptions{})
	// An upload that ended without the rest of the download (refused, or
	// failed) leaves it mid-write; this ends the write, and the download.
	_ = pr.CloseWithError(errUploadEnded)
	<-done

	var infected *avscan.Infected
	switch {
	case capped.over:
		return storedFile{Outcome: fileTooLarge}, nil
	case errors.Is(fetchErr, importProvider.ErrAttachmentGone):
		return storedFile{Outcome: fileGone}, nil
	case fetchErr != nil && !errors.Is(fetchErr, errUploadEnded):
		return storedFile{}, fetchErr // the provider's: a rate limit is waited out
	case errors.Is(err, uploadsafe.ErrEmptyBody):
		return storedFile{Outcome: fileEmpty}, nil
	case errors.As(err, &infected):
		return storedFile{Outcome: fileInfected, Signature: infected.Signature}, nil
	case err != nil:
		return storedFile{}, fmt.Errorf("keep the file: %w", err)
	}
	if err := createAttachment(ctx, kept.ID, kept.ObjKey, by, srcKey, srcValue); err != nil {
		if rerr := removeStored(ctx, kept.ObjKey); rerr != nil {
			helpers.LogErrorWithContext(ctx, "business/Import/storeFile: %s has no attachments row and stays stored: %v", kept.ObjKey, rerr)
		}
		return storedFile{}, fmt.Errorf("create attachment row: %w", err)
	}
	return storedFile{ID: kept.ID, ObjKey: kept.ObjKey, Size: kept.Size, ContentType: kept.ContentType}, nil
}

// cappedWriter writes up to left bytes, and refuses the rest.
type cappedWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		c.over = true
		return 0, errOverLimit
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}
