package controllers

// Call Quality Telemetry's routes (calls A5, #56):
//   - POST /calls/{session_id}/quality: a member's own samples of the call;
//   - POST /guest/calls/quality: a guest's own, by their call token;
//   - GET /calls/{session_id}/quality: why the call stuttered, per person,
//     for whoever runs the call and for admins;
//   - GET /calls/quality/days?days=30: the install's days, for admins.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	callQualityBusiness "github.com/akashc777/OneCamp/business/CallQuality"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

type qualityBatch struct {
	Samples []callQualityBusiness.Sample `json:"samples"`
}

// RecordCallQuality POST /calls/{session_id}/quality {samples}: a member's
// own samples of the live call.
func RecordCallQuality(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	var b qualityBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	n, err := callQualityBusiness.Record(r.Context(), &userInfo.UserDgraphInfo, chi.URLParam(r, "session_id"), b.Samples)
	answerQuality(w, r, n, err)
}

// GuestRecordCallQuality POST /guest/calls/quality {samples}: a guest's own
// samples of the call their call token is for.
func GuestRecordCallQuality(w http.ResponseWriter, r *http.Request) {
	var b qualityBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	n, err := callQualityBusiness.RecordGuest(r.Context(), r.Header.Get("Authorization"), b.Samples)
	answerQuality(w, r, n, err)
}

func answerQuality(w http.ResponseWriter, r *http.Request, n int, err error) {
	switch {
	case errors.Is(err, callQualityBusiness.ErrTooMany):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"code": "too_many", "msg": "Send between one and twelve samples at a time."})
	case errors.Is(err, callQualityBusiness.ErrThroughToken):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "through_token", "msg": "Only the app in the call sends its quality."})
	case errors.Is(err, callQualityBusiness.ErrNotInCall):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "not_in_call", "msg": "You're not in this call now."})
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/quality: keeping samples failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't keep that just now.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"kept": n}})
	}
}

// CallQuality GET /calls/{session_id}/quality: why the call stuttered for each
// person in it, for whoever runs it and for admins.
func CallQuality(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	people, err := callQualityBusiness.Why(r.Context(), &userInfo.UserDgraphInfo, userInfo.UserPostgresInfo.IsAdmin, chi.URLParam(r, "session_id"))
	switch {
	case errors.Is(err, callQualityBusiness.ErrNotAllowed), errors.Is(err, callQualityBusiness.ErrThroughToken):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_allowed", "msg": "Only whoever runs the call, or an admin, sees how it went for each person."})
	case errors.Is(err, callQualityBusiness.ErrNotInCall):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "not_found", "msg": "That call isn't here."})
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/CallQuality failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't read that just now.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": people})
	}
}

// Seam for tests: the install's days.
var qualityDays = callQualityBusiness.Days

// CallQualityDays GET /calls/quality/days?days=30: how the install's calls
// went, a row a day, for admins acting themselves.
func CallQualityDays(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	if !userInfo.UserPostgresInfo.IsAdmin || helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_allowed", "msg": "Only an admin sees how the workspace's calls went."})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if n == 0 {
		n = 30
	}
	days, err := qualityDays(r.Context(), n, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/CallQualityDays failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't read that just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": days})
}
