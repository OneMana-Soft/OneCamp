package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
)

func failedUploadAnswer(err error) (int, string) {
	w := httptest.NewRecorder()
	WriteUploadFailure(context.Background(), w, err, "The file couldn't be saved. Try again.")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	msg, _ := body["msg"].(string)
	return w.Code, msg
}

func storageCheckSays(t *testing.T, err error) {
	t.Helper()
	withChecks(t, SystemCheck{Name: "storage", Kind: CheckKindDependency, Probe: func(context.Context) error { return err }})
}

// An upload that failed because storage is down says so, with a 503 and words
// the web shows as they are; one that failed otherwise is a 500 with the
// handler's own words. Neither is the 400 that told people to check their
// request.
func TestAFailedUploadSaysWhetherStorageIsDown(t *testing.T) {
	writeFailed := errors.New("dial tcp minio:9000: connect: connection refused")

	storageCheckSays(t, errors.New("could not reach MinIO"))
	if code, msg := failedUploadAnswer(writeFailed); code != http.StatusServiceUnavailable || msg != "File storage isn't answering right now. Try again in a moment." {
		t.Errorf("storage down: %d %q", code, msg)
	}

	storageCheckSays(t, nil)
	if code, msg := failedUploadAnswer(writeFailed); code != http.StatusInternalServerError || msg != "The file couldn't be saved. Try again." {
		t.Errorf("storage up: %d %q", code, msg)
	}

	// Nothing registered under the name: nothing says it is down.
	withChecks(t)
	if !dependencyAnswers(context.Background(), "storage") {
		t.Error("an unregistered check was taken for a failing one")
	}
}

// A file refused for what it is stays the uploader's to deal with, and says
// why, whatever storage is doing: the errors are the ones the upload path
// really returns, wrapped the way it wraps them.
func TestARefusedFileSaysWhyItWasRefused(t *testing.T) {
	storageCheckSays(t, errors.New("could not reach MinIO"))

	_, empty := uploadsafe.PrepareUpload(strings.NewReader(""), "notes.txt", uploadsafe.Options{})
	_, exe := uploadsafe.PrepareUpload(strings.NewReader("MZ\x90\x00"), "setup.exe", uploadsafe.Options{AllowedExtensions: []string{".png", ".jpg"}})
	for _, c := range []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"empty", fmt.Errorf("upload validation: %w", empty), http.StatusBadRequest, "The file is empty."},
		{"kind", fmt.Errorf("upload validation: %w", exe), http.StatusUnsupportedMediaType, "Files of this kind can't be uploaded."},
		{"virus", fmt.Errorf("upload blocked: %w (%s)", avscan.ErrInfected, "Eicar-Signature"), http.StatusUnprocessableEntity, "This file was blocked because it looks like it carries a virus."},
	} {
		if c.err == nil || errors.Unwrap(c.err) == nil {
			t.Fatalf("%s: the upload path no longer refuses this file", c.name)
		}
		if code, msg := failedUploadAnswer(c.err); code != c.code || msg != c.msg {
			t.Errorf("%s: %d %q, want %d %q", c.name, code, msg, c.code, c.msg)
		}
	}
}
