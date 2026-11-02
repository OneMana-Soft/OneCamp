package controllers

// A meeting's recording (meet-02, head D): an event's call is recorded like a
// conversation's, by the ONE start (business/LiveKit StartCallRecording), and
// only by whoever runs it by rule (HostByRule: the meeting's owner, an event's
// organiser), the way its lobby is changed.

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// Seams for tests.
var (
	meetingHost        = liveKitBusiness.HostByRule
	startCallRecording = liveKitBusiness.StartCallRecording
	stopCallRecording  = liveKitBusiness.StopRecording
	recordAudit        = auditBusiness.Record
)

// SetMeetingRecording POST /calls/meetings/{meeting_id}/recording {"on": bool}
// starts or stops recording a meeting's call: an event's call keeps its
// recording on the event, for whoever may read the event. Whoever runs the
// meeting by rule only (its owner; an event's organiser), as its lobby is
// changed, and never through an API token. A meeting that keeps nothing (an
// instant meeting, a personal room) answers 409; a stop with nothing recording
// answers as on every route that stops one (httpx.WriteNothingRecording).
func SetMeetingRecording(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Record a call in the app, not through an API token."})
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.On == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	id := chi.URLParam(r, "meeting_id")
	room := callStateBusiness.RoomOf(callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: id})
	host, err := meetingHost(r.Context(), room, &userInfo.UserDgraphInfo, false)
	if err != nil {
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if !host {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That meeting isn't here."})
		return
	}
	summary := "Started recording a meeting's call"
	if *body.On {
		err = startCallRecording(r.Context(), room, &userInfo.UserDgraphInfo)
	} else {
		summary = "Stopped recording a meeting's call"
		err = stopCallRecording(r.Context(), room)
	}
	if httpx.WriteNothingRecording(w, err) {
		return
	}
	switch {
	case errors.Is(err, liveKitBusiness.ErrTranscriptNotKept):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This call keeps no recordings: only a conversation's or an event's call is recorded."})
		return
	case err != nil:
		helpers.WriteRetryLater(w, "The call server couldn't change the recording just now.")
		return
	}
	recordAudit(r, "call.recording", auditBusiness.CategoryCalls, summary, map[string]interface{}{"meeting": id, "on": *body.On})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"on": *body.On}})
}
