package controllers

// "Why did or didn't I get this?" (A4 H3) over HTTP: Settings >
// Notifications' "Send me a test" and each Inbox item's "Why am I seeing
// this?", both the router's (business/Notify Test and Why).
//
// Permission model: any member, guests included, for themselves alone. Whom
// a test reaches and whose record is read is the session's person, never
// anyone the request names; another person's item answers 404, the same as
// one with no record, so nothing confirms it exists. Neither answer carries a
// body. The demo's shared visitor sends no test (router/chatNotify.go).

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Seams for tests: the router's test notice and its record.
var (
	testNotice = notifyBusiness.Test
	whyOf      = notifyBusiness.Why
)

// sessionPerson is the signed-in person's id; with no session it answers 401 and false.
func sessionPerson(w http.ResponseWriter, r *http.Request) (string, bool) {
	info, ok := httpx.MustUser(w, r)
	if !ok {
		return "", false
	}
	if info.UserPostgresInfo.Id == uuid.Nil {
		helpers.WriteUnauthorized(w)
		return "", false
	}
	return info.UserPostgresInfo.Id.String(), true
}

// SendMeATest is POST /notify/test: a test notice to the session's own
// devices through the router, answered step by step (where it can show, the
// hold, what was told). One a minute: a second answers 429 with Retry-After
// and retry_at.
func SendMeATest(w http.ResponseWriter, r *http.Request) {
	me, ok := sessionPerson(w, r)
	if !ok {
		return
	}
	now := time.Now()
	res, err := testNotice(r.Context(), me, now)
	var soon *notifyBusiness.TooSoon
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, res)
	case errors.As(err, &soon):
		wait := max(1, int(math.Ceil(soon.RetryAt.Sub(now).Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{
			"code": "too_soon", "retry_at": soon.RetryAt.UTC(), "msg": "One test a minute. Try again in a moment.",
		})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/SendMeATest err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send a test. Try again."})
	}
}

// WhyAmISeeingThis is GET /notify/why/{message_id}: the session's own record
// of the message an Inbox item is about, one trace per event that reached
// them. One that told them nothing answers 404.
func WhyAmISeeingThis(w http.ResponseWriter, r *http.Request) {
	me, ok := sessionPerson(w, r)
	if !ok {
		return
	}
	events, err := whyOf(r.Context(), me, chi.URLParam(r, "message_id"))
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"events": events})
	case errors.Is(err, notifyBusiness.ErrNoRecord):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_record", "msg": "There's no record of how this reached you."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/WhyAmISeeingThis err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't look that up. Try again."})
	}
}
