package helpers

import (
	"context"
	"errors"
	"net/http"
)

// Refusal is something the server won't do, in words the person can act on:
// Code is what the client acts on, Msg what the person reads, and Status what
// it is answered with (403 when unset). Every endpoint that runs into one
// answers it with WriteRefusal, so a new kind of refusal needs no change
// there. Calls refuse with it (a host put someone out, the call is locked,
// later the lobby or a wrong passcode), and so does a sensitivity label
// (business/Label, code "label_forbids").
type Refusal struct {
	Code string
	Msg  string
	// Status is what it is answered with: 403 when unset. A refusal that
	// passes (the call couldn't be checked just now) is a 503 to try again.
	Status int
}

// CallRefusal is a call that won't let someone in: a Refusal. Every endpoint
// that hands out a way into a call (a channel's, a DM's or a group's token, an
// instant meeting's, a guest link's) answers one with WriteCallRefusal.
type CallRefusal = Refusal

func (e *Refusal) Error() string { return e.Msg }

// WriteRefusal answers a request that ran into a Refusal with its status
// (403 unless it says otherwise), carrying its code and words, and reports
// whether it did.
func WriteRefusal(w http.ResponseWriter, err error) bool {
	var e *Refusal
	if !errors.As(err, &e) {
		return false
	}
	status := e.Status
	if status == 0 {
		status = http.StatusForbidden
	}
	WriteJSON(w, status, Envolope{"code": e.Code, "msg": e.Msg})
	return true
}

// WriteCallRefusal is WriteRefusal, for a join that ran into a CallRefusal.
func WriteCallRefusal(w http.ResponseWriter, err error) bool { return WriteRefusal(w, err) }

// RefusedUnless answers a check that read the store to decide whether someone
// may name something (a channel, a project) on a form or through an install.
// ok and err are what that read answered. It is nil when the read said yes,
// and refusal itself when it said no or found nothing by that id (ErrNotFound:
// what isn't there is refused the same way), so a caller's == and errors.Is on
// refusal still hold.
//
// A read that failed is ours, not the person's (V-1281): where names the check,
// and the cause is logged there. It still refuses (nothing is saved on a read
// that failed), with an error that reads as refusal does, so a form that
// answers its error's words says what it always said, but is no Refusal, so an
// install answers it 500 in its own words, never 400 "choose ...". It keeps
// no cause to unwrap: the cause is in the log, never in an answer.
func RefusedUnless(ctx context.Context, where string, ok bool, err error, refusal error) error {
	switch {
	case err != nil && !errors.Is(err, ErrNotFound):
		LogErrorWithContext(ctx, "%s couldn't check, so it refused: %+v", where, err)
		return &checkFailed{words: refusal.Error()}
	case ok && err == nil:
		return nil
	}
	return refusal
}

// checkFailed is a check whose read failed: the refusal's words, not its kind.
type checkFailed struct{ words string }

func (e *checkFailed) Error() string { return e.words }
