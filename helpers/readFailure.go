package helpers

import (
	"context"
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

// ErrConflict is a request that clashed with what is already there: an
// address another booking page uses, or a change someone made at the same
// moment, which the write lost to even after trying again. It is the person's
// to sort out (pick another address, try again), so it is answered with a 409
// and a sentence that says what clashed, never a 5xx.
var ErrConflict = errors.New("conflict")

// conflict is a clash in a sentence for the person, carrying what caused it
// (a unique violation, Dgraph's abort) for errors.Is and the logs.
type conflict struct {
	text  string
	cause error
}

func (e *conflict) Error() string {
	if e.cause == nil {
		return e.text
	}
	return e.text + " (" + e.cause.Error() + ")"
}

// Is matches every Conflict error to ErrConflict.
func (e *conflict) Is(target error) bool { return target == ErrConflict }

func (e *conflict) Unwrap() error { return e.cause }

// Conflict makes an ErrConflict that says what clashed, in words written for
// the person ("Another booking page already uses the address "standup". Pick
// another."), over the error that found it, which errors.Is still matches.
func Conflict(text string, cause error) error { return &conflict{text: text, cause: cause} }

// ConcurrentChangeText is what a write that lost to a change made at the same
// moment says, when the store gave up trying it again (Dgraph's abort after
// dgraphInit's retries).
const ConcurrentChangeText = "Someone changed this at the same moment. Try again."

// conflictText is the sentence a Conflict error carries for the person.
func conflictText(err error) (string, bool) {
	var c *conflict
	if errors.As(err, &c) {
		return c.text, true
	}
	return "", false
}

// ClientWentAway reports whether ctx is a request's whose client stopped
// waiting: the browser closed the panel or the tab, or went to another page,
// and the server called the request off (net/http cancels its context when
// the connection goes). What fails after that isn't a failure anyone sees:
// the log helper writes it at DEBUG, and middleware.ClientGone records the
// answer as 499, not a 5xx. A deadline the server set is DeadlineExceeded, a
// failure of its own, and isn't this.
func ClientWentAway(ctx context.Context) bool {
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled)
}

// StatusClientClosedRequest is the status a request is recorded with when its
// client went away before it was answered (nginx's 499). Nothing receives it.
const StatusClientClosedRequest = 499

// WriteReadFailure answers a read of one thing (what: "task", "project") that
// failed, and its handler then returns. Nothing by that id for this reader
// (an error that is ErrNotFound) is a 404, "Task not found"; a clash
// (ErrConflict) is a 409 in its own words; anything else is a failed read,
// logged under where, and a 503 the app offers Try again for.
//
// An id that names nothing is never a 400: the app can only read a 400 as a
// request it got wrong, and it said "a connection problem, try again" about a
// task that was never there (PTC-4).
func WriteReadFailure(w http.ResponseWriter, r *http.Request, err error, what, where string) {
	if errors.Is(err, ErrNotFound) {
		WriteJSON(w, http.StatusNotFound, Envolope{"msg": upperFirst(what) + " not found"})
		return
	}
	if writeConflict(w, err) {
		return
	}
	LogReadFailure(r.Context(), err, where+" couldn't read the "+what)
	WriteRetryLater(w, "Couldn't load the "+what+".")
}

// WriteWriteFailure answers a change that failed, and its handler then
// returns. A clash (ErrConflict: an address someone else uses, or a change
// made at the same moment that the write lost to after its retries) is a 409
// in the conflict's own words; anything else is logged under where and
// answered 503 with couldnt ("Couldn't move the task.") and the advice to try
// again.
func WriteWriteFailure(w http.ResponseWriter, r *http.Request, err error, couldnt, where string) {
	if writeConflict(w, err) {
		return
	}
	LogErrorWithContext(r.Context(), "%s: %+v", where, err)
	WriteRetryLater(w, couldnt)
}

// writeConflict answers a Conflict error with a 409 in its own words, and
// says whether it did.
func writeConflict(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, ErrConflict) {
		return false
	}
	text, ok := conflictText(err)
	if !ok {
		text = ConcurrentChangeText
	}
	WriteJSON(w, http.StatusConflict, Envolope{"msg": text})
	return true
}

// LogReadFailure logs a read of one thing that failed, under where: the store
// didn't answer, or its answer couldn't be read. A read that found nothing
// (ErrNotFound) is an answer, not a failure, and isn't logged: a doc deleted
// for good a moment ago is an ordinary find for a sweep that still had work
// for it, or for a link opened after it went, and only the caller can say
// whether it was expected (WriteReadFailure answers it with a 404). It says
// whether it logged.
func LogReadFailure(ctx context.Context, err error, where string) bool {
	if err == nil || errors.Is(err, ErrNotFound) {
		return false
	}
	LogErrorWithContext(ctx, "%s: %+v", where, err)
	return true
}

// upperFirst is s with its first letter in capitals ("task" -> "Task").
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
