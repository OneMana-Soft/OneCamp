package controllers

// The call's thread surface (calls CALLS-3b):
//   - GET and POST /calls/{session_id}/thread: a member's, someone in its
//     conversation or let into the call by its invite;
//   - GET and POST /guest/calls/thread: a guest's, by the call token they
//     joined with.
// The reads answer the call's post and what was written while the call went
// on, and nothing of the conversation, with whether the reader may write; a
// write is plain text, {"text": "..."}. A call that's over, has no post yet,
// or isn't theirs answers not found.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// CallThread GET /calls/{session_id}/thread[?since=].
func CallThread(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	t, err := callThreadBusiness.ForMember(r.Context(), &userInfo, chi.URLParam(r, "session_id"), time.Now(), pollSince(r))
	answerCallThread(w, r, t, err)
}

// GuestCallThread GET /guest/calls/thread[?since=].
func GuestCallThread(w http.ResponseWriter, r *http.Request) {
	t, err := callThreadBusiness.ForGuest(r.Context(), r.Header.Get("Authorization"), time.Now(), pollSince(r))
	answerCallThread(w, r, t, err)
}

// pollSince is a poll's since: the newest reply the app has (RFC 3339). One
// it can't read asks for the whole thread. Pure.
func pollSince(r *http.Request) time.Time {
	since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
	if err != nil {
		return time.Time{}
	}
	return since
}

// callThreadReply is a reply's body.
type callThreadReply struct {
	Text string `json:"text"`
}

// ReplyInCallThread POST /calls/{session_id}/thread.
func ReplyInCallThread(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	var in callThreadReply
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	answerCallThreadReply(w, r, callThreadBusiness.ReplyAsMember(r.Context(), &userInfo, chi.URLParam(r, "session_id"), in.Text))
}

// GuestReplyInCallThread POST /guest/calls/thread.
func GuestReplyInCallThread(w http.ResponseWriter, r *http.Request) {
	var in callThreadReply
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	answerCallThreadReply(w, r, callThreadBusiness.ReplyAsGuest(r.Context(), r.Header.Get("Authorization"), in.Text))
}

// answerCallThreadReply answers a write: sent, the writer's own mistake in
// their words, a thread they may only read, one that's gone (as a read
// answers it), or a retry.
func answerCallThreadReply(w http.ResponseWriter, r *http.Request, err error) {
	var input *guestBusiness.ErrGuestInput
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
	case errors.As(err, &input):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": input.Msg})
	case errors.Is(err, callThreadBusiness.ErrReadOnly):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "read_only", "msg": "You can read this call's chat, but not write in it."})
	case errors.Is(err, callThreadBusiness.ErrChatHeld):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "chat_held", "msg": callThreadBusiness.ChatHeldText})
	case errors.Is(err, callThreadBusiness.ErrNotFound), errors.Is(err, guestBusiness.ErrNotFound):
		answerCallThread(w, r, nil, err)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/callThread reply failed: %+v", err)
		helpers.WriteRetryLater(w, "Your message couldn't be sent just now.")
	}
}

func answerCallThread(w http.ResponseWriter, r *http.Request, t *guestBusiness.CallThread, err error) {
	switch {
	case errors.Is(err, callThreadBusiness.ErrNotFound), errors.Is(err, guestBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "not_found", "msg": "This call's chat isn't here. The call may have ended."})
	case errors.Is(err, callThreadBusiness.ErrUnchanged):
		// Nothing new: an empty answer, so most polls carry nothing.
		w.WriteHeader(http.StatusNoContent)
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/callThread failed: %+v", err)
		helpers.WriteRetryLater(w, "The call's chat couldn't be read just now.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
	}
}
