package helpers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
)

// dependencyProbeTimeout bounds dependencyAnswers: it runs while a person
// waits for an answer to their upload.
const dependencyProbeTimeout = 3 * time.Second

// dependencyAnswers reports whether the named dependency check (one of the
// checks behind /health/ready and Admin > System checks: "storage",
// "realtime", …) passes right now. A name nobody registered answers true:
// nothing says it is down.
func dependencyAnswers(ctx context.Context, name string) bool {
	systemCheckMu.RLock()
	c, ok := systemChecks[name]
	systemCheckMu.RUnlock()
	if !ok {
		return true
	}
	return runOneCheck(ctx, c, dependencyProbeTimeout).Healthy
}

// storageDownWords is what a person is told when file storage isn't answering.
const storageDownWords = "File storage isn't answering right now."

// WriteStorageFailure answers a request whose write to file storage failed.
//
// WHY. An upload that failed because MinIO was down was answered 400 "Failed
// to upload the file" in a shape the web doesn't read, so people were told
// "Something in the request wasn't accepted. Check it and try again": their
// fault, and nothing to check. When the storage check says storage isn't
// answering, which is nothing the caller did and passes, this answers 503
// with words that say so and to try again in a moment; otherwise 500 with
// msg. Either way the words are in "msg", which the web shows as they are.
func WriteStorageFailure(ctx context.Context, w http.ResponseWriter, msg string) {
	if !dependencyAnswers(ctx, "storage") {
		WriteRetryLater(w, storageDownWords)
		return
	}
	WriteJSON(w, http.StatusInternalServerError, Envolope{"msg": msg})
}

// WriteUploadFailure answers an upload that was refused or not stored. A file
// refused for what it is (empty, a kind that may not be uploaded, or carrying a
// virus) is the uploader's to deal with: 4xx, with words saying which. Any
// other failure is the server's, answered by WriteStorageFailure with msg.
func WriteUploadFailure(ctx context.Context, w http.ResponseWriter, err error, msg string) {
	switch {
	case errors.Is(err, uploadsafe.ErrEmptyBody):
		WriteJSON(w, http.StatusBadRequest, Envolope{"msg": "The file is empty."})
	case errors.Is(err, uploadsafe.ErrUnsupportedType), errors.Is(err, uploadsafe.ErrSignatureMismatch):
		WriteJSON(w, http.StatusUnsupportedMediaType, Envolope{"msg": "Files of this kind can't be uploaded."})
	case errors.Is(err, avscan.ErrInfected):
		WriteJSON(w, http.StatusUnprocessableEntity, Envolope{"msg": "This file was blocked because it looks like it carries a virus."})
	default:
		WriteStorageFailure(ctx, w, msg)
	}
}
