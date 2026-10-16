package helpers

import (
	"errors"
	"net/http"
	"strings"
)

// ErrNotFound is "nothing by that id" for a read of one thing: it never
// existed, it was deleted, or (where the access rules hide it) the reader may
// not know it is there. A model's own "found nothing" error is made with
// NotFound, so it stays its own sentinel for errors.Is and also matches this
// one, and WriteReadFailure answers every such read the same way.
var ErrNotFound = errors.New("not found")

// notFound is a "found nothing" error in its own words. A pointer, so two
// with the same words are still two sentinels.
type notFound struct{ text string }

func (e *notFound) Error() string { return e.text }

// Is matches every NotFound error to ErrNotFound.
func (e *notFound) Is(target error) bool { return target == ErrNotFound }

// NotFound makes a sentinel for a read that found nothing, with the words the
// read always gave, which errors.Is also matches to ErrNotFound:
//
//	var ErrNotFound = helpers.NotFound("failed to get dgraph task")
func NotFound(text string) error { return &notFound{text: text} }

// WriteReadFailure answers a read of one thing (what: "task", "project") that
// failed, and its handler then returns. Nothing by that id for this reader
// (an error that is ErrNotFound) is a 404, "Task not found"; anything else is
// a failed read, logged under where, and a 503 the app offers Try again for.
//
// An id that names nothing is never a 400: the app can only read a 400 as a
// request it got wrong, and it said "a connection problem, try again" about a
// task that was never there (PTC-4).
func WriteReadFailure(w http.ResponseWriter, r *http.Request, err error, what, where string) {
	if errors.Is(err, ErrNotFound) {
		WriteJSON(w, http.StatusNotFound, Envolope{"msg": upperFirst(what) + " not found"})
		return
	}
	LogErrorWithContext(r.Context(), "%s couldn't read the %s: %+v", where, what, err)
	WriteRetryLater(w, "Couldn't load the "+what+".")
}

// upperFirst is s with its first letter in capitals ("task" -> "Task").
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
