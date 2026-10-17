package helpers

import (
	"errors"
	"net/http"
)

// DocNotFound is the one answer to a doc that isn't there for the person:
// one that doesn't exist, and one they can't open. A 404 never tells the two
// apart (a doc someone can't open is answered as one that isn't there), so
// the sentence says both, and every answer about a missing doc uses it:
// people's, a guest's and an agent's alike. docNotFoundGuard_test.go fails on
// any other wording.
const DocNotFound = "This doc doesn't exist, or you can't open it."

// WriteDocNotFound is the 404 for a doc that isn't there for the person.
func WriteDocNotFound(w http.ResponseWriter) {
	WriteJSON(w, http.StatusNotFound, Envolope{"msg": DocNotFound})
}

// WriteDocReadFailure answers a read of one doc, or a decision about one,
// that gave no answer: nothing there for the person (WriteDocNotFound, 404),
// or a read that failed, logged under where and answered as one to try again
// (WriteReadFailure, 503). Every doc answer of that kind goes through it, so
// a doc's 404 is worded once.
func WriteDocReadFailure(w http.ResponseWriter, r *http.Request, err error, where string) {
	if errors.Is(err, ErrNotFound) {
		WriteDocNotFound(w)
		return
	}
	WriteReadFailure(w, r, err, "doc", where)
}
