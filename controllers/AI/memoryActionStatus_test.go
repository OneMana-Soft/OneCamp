package controllers

import (
	"errors"
	"net/http"
	"testing"
)

// The memory actions answer with the status that says what went wrong, read from the business layer's own words.
func TestMemoryActionStatusSaysWhatWentWrong(t *testing.T) {
	for msg, want := range map[string]int{
		"not authorized for this memory item":  http.StatusForbidden,
		"you must be an admin to resolve this": http.StatusForbidden,
		"memory item not found":                http.StatusNotFound,
		"workspace memory is not enabled":      http.StatusConflict,
		"a commitment needs a due date":        http.StatusBadRequest,
	} {
		if got := memoryActionStatus(errors.New(msg)); got != want {
			t.Errorf("%q is answered %d, want %d", msg, got, want)
		}
	}
}
