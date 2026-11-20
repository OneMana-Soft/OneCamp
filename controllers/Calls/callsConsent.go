package controllers

// A person's own answer to a call's recording or transcript notice (zoom-09):
// OK or Leave, written to the admin audit log as who agreed to what was on.
// Who they are is what the server proved (a member's session, a guest's own
// call token), never what the body says; what was on is what the call's room
// says when the row is written. The body carries only the choice and what the
// notice showed, so a stale notice can't agree to something else.

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// RecordCallConsent POST /calls/{session_id}/consent {choice, recording,
// transcript}: a member in the call answers its notice.
func RecordCallConsent(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	c, ok := readConsent(w, r)
	if !ok {
		return
	}
	recorded, err := callControlBusiness.MemberConsents(r.Context(), userInfo.UserDgraphInfo.Uid, chi.URLParam(r, "session_id"), c, privacyWriter(r))
	answerConsent(w, r, recorded, err)
}

// GuestRecordCallConsent POST /guest/calls/consent: the same for a guest,
// proved by their own call token (Authorization: Bearer <token>).
func GuestRecordCallConsent(w http.ResponseWriter, r *http.Request) {
	c, ok := readConsent(w, r)
	if !ok {
		return
	}
	recorded, err := callControlBusiness.GuestConsents(r.Context(), r.Header.Get("Authorization"), c, privacyWriter(r))
	answerConsent(w, r, recorded, err)
}

func readConsent(w http.ResponseWriter, r *http.Request) (callControlBusiness.Consent, bool) {
	var c callControlBusiness.Consent
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return c, false
	}
	return c, true
}

// privacyWriter writes a call's privacy row for this request: its address and
// browser, and the member's session when there is one.
func privacyWriter(r *http.Request) callControlBusiness.PrivacyWriter {
	return func(kind, summary string, meta map[string]interface{}) error {
		return auditBusiness.RecordCallPrivacy(r, kind, summary, meta)
	}
}

// answerConsent says whether a row was written (false: the same answer was
// written already), or why not.
func answerConsent(w http.ResponseWriter, r *http.Request, recorded bool, err error) {
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls consent failed: %+v", err)
		helpers.WriteRetryLater(w, "Your answer couldn't be saved just now. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]bool{"recorded": recorded}})
}
