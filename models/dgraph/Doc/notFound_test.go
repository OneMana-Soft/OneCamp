package models

import (
	"errors"
	"fmt"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// A doc read that found nothing is the shared "not found", so a handler
// answers it as a 404 (helpers.WriteReadFailure), never as a failed read; and
// it stays the doc's own sentinel, with its own words.
func TestNotFoundIsTheSharedNotFound(t *testing.T) {
	if !errors.Is(ErrNotFound, helpers.ErrNotFound) || !errors.Is(fmt.Errorf("reading a doc: %w", ErrNotFound), helpers.ErrNotFound) {
		t.Fatal("ErrNotFound, or an error wrapping it, isn't helpers.ErrNotFound")
	}
	if errors.Is(helpers.NotFound("failed to get dgraph doc"), ErrNotFound) {
		t.Error("another NotFound with the same words matched the doc's ErrNotFound")
	}
	if ErrNotFound.Error() != "failed to get dgraph doc" {
		t.Errorf("ErrNotFound lost its words: %q", ErrNotFound.Error())
	}
}
