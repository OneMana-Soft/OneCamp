package models

import (
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// A project read that found nothing is the shared "not found", so a handler
// answers it as a 404 (helpers.WriteReadFailure), never as a failed read.
func TestNotFoundIsTheSharedNotFound(t *testing.T) {
	if !errors.Is(ErrNotFound, helpers.ErrNotFound) {
		t.Fatal("ErrNotFound isn't helpers.ErrNotFound")
	}
	if ErrNotFound.Error() != "failed to get dgraph project" {
		t.Errorf("ErrNotFound lost its words: %q", ErrNotFound.Error())
	}
}
