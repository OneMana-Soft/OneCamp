package helpers

import (
	"errors"
	"net/http"
)

// CallRefusal is a call that won't let someone in, in words they can act on:
// a host put them out of it, it is locked, and later, the lobby or a wrong
// passcode. Code is what the client acts on, Msg what the person reads.
//
// Every endpoint that hands out a way into a call (a channel's, a DM's or a
// group's token, an instant meeting's, a guest link's) answers a refusal with
// WriteCallRefusal, so a new kind of refusal needs no change there.
type CallRefusal struct {
	Code string
	Msg  string
	// Status is what it is answered with: 403 when unset. A refusal that
	// passes (the call couldn't be checked just now) is a 503 to try again.
	Status int
}

func (e *CallRefusal) Error() string { return e.Msg }

// WriteCallRefusal answers a join that ran into a CallRefusal with its status
// (403 unless it says otherwise), carrying its code and words, and reports
// whether it did.
func WriteCallRefusal(w http.ResponseWriter, err error) bool {
	var e *CallRefusal
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
