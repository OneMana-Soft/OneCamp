package Calendar

// A guest's own page over HTTP (business/Calendar rsvp.go): someone invited by
// email reads their invitation and answers it, with no account. Public, each
// route with its own rate limit (router.go, CAL9), and every "not available"
// answered alike: 404 with one message.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

func writeInvitationError(w http.ResponseWriter, r *http.Request, where string, err error) {
	var input *business.EventInputError
	switch {
	case errors.Is(err, business.ErrInvitationGone):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": business.ErrInvitationGone.Error()})
	case errors.Is(err, business.ErrMoving):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Your invitation can't be opened for a minute while the calendar is updated. Try again shortly.", "code": "calendar_moving"})
	case errors.As(err, &input):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": input.Error()})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calendar/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Your invitation couldn't be opened just now. Try again in a moment."})
	}
}

// GetPublicInvitation is a guest's invitation. GET /public/rsvp/{token}
func GetPublicInvitation(w http.ResponseWriter, r *http.Request) {
	v, err := business.Invitation(r.Context(), chi.URLParam(r, "token"), time.Now())
	if err != nil {
		writeInvitationError(w, r, "GetPublicInvitation", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// AnswerPublicInvitation is a guest's answer from their link.
// POST /public/rsvp/{token} {answer, note}
func AnswerPublicInvitation(w http.ResponseWriter, r *http.Request) {
	var in business.AnswerInput
	if err := json.NewDecoder(io.LimitReader(r.Body, answerBodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that answer."})
		return
	}
	in.Scope = ""
	v, err := business.AnswerInvitation(r.Context(), chi.URLParam(r, "token"), in, time.Now())
	if err != nil {
		writeInvitationError(w, r, "AnswerPublicInvitation", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}
