package helpers

import (
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
