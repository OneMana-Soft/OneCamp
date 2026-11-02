package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// AddCalendarEventCall gives an event its own call (calendar B11), or answers
// the one it has: where its guests come in. For whoever may change the event.
// POST /calendar/events/{eventId}/call
func AddCalendarEventCall(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	call, err := business.Events.AddCall(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, "AddCalendarEventCall", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": call})
}

// RemoveCalendarEventCall takes an event's call off and ends it. For whoever
// may change the event. DELETE /calendar/events/{eventId}/call
func RemoveCalendarEventCall(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	if err := business.Events.RemoveCall(r.Context(), who, chi.URLParam(r, "eventId")); err != nil {
		writeEventCallError(w, r, "RemoveCalendarEventCall", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "The call was taken off this event."})
}

// GetCalendarEventCallArtefacts lists what an event's call made (meet-02):
// each finished call that kept something, newest first, with its transcript,
// its notes document and the conversation its recap went to. For whoever may
// read the event, by the calendar's own rule; anyone else is told there is no
// such event. GET /calendar/events/{eventId}/call/artefacts
func GetCalendarEventCallArtefacts(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	keys, err := business.Events.CallArtefactKeys(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, "GetCalendarEventCallArtefacts", err)
		return
	}
	list, err := callStateBusiness.EventArtefacts(r.Context(), keys)
	if err != nil {
		writeEventCallError(w, r, "GetCalendarEventCallArtefacts", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": list})
}

// eventRecording finds a recording an event's call made (a seam for tests).
var eventRecording = callStateBusiness.EventRecording

// GetCalendarEventCallRecording answers the playback link of a recording an
// event's call made (meet-02), for whoever may read the event by the
// calendar's own rule, as its "Recordings & notes" are; anyone else is told
// there is no such event, and a key that is no recording of this event's is
// not found. GET /calendar/events/{eventId}/call/artefacts/{key}/recording
func GetCalendarEventCallRecording(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	keys, err := business.Events.CallArtefactKeys(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, "GetCalendarEventCallRecording", err)
		return
	}
	rec, err := eventRecording(r.Context(), keys, chi.URLParam(r, "key"))
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetCalendarEventCallRecording: %+v", err)
		helpers.WriteRetryLater(w, httpx.RecordingUnavailable)
		return
	}
	if rec == nil {
		httpx.WriteRecordingRefusal(w, http.StatusNotFound)
		return
	}
	httpx.WriteRecordingURL(w, r, rec)
}

// eventTranscript reads a page of the transcript of a recording an event's
// call made (a seam for tests).
var eventTranscript = callStateBusiness.EventTranscript

// A transcript's page: 20 lines unless asked, at most 100, as a
// conversation's recording transcript pages.
const (
	transcriptPageSize = 20
	transcriptPageMax  = 100
)

// GetCalendarEventCallTranscript answers a page of the transcript of a
// recording an event's call made (meet-02), for whoever may read the event by
// the calendar's own rule, as its recording plays: the lines in the order
// they were said (pageIndex from 0, pageSize), their count, and how many
// pages they fill, in the shape a conversation's recording transcript
// answers, so the one player reads both. Anyone else is told there is no
// such event, and a key that is no recording of this event's is not found.
// GET /calendar/events/{eventId}/call/artefacts/{key}/transcript
func GetCalendarEventCallTranscript(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	keys, err := business.Events.CallArtefactKeys(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, "GetCalendarEventCallTranscript", err)
		return
	}
	page := helpers.PageParam(r.URL.Query(), transcriptPageSize, transcriptPageMax)
	rec, err := eventTranscript(r.Context(), keys, chi.URLParam(r, "key"), page)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetCalendarEventCallTranscript: %+v", err)
		helpers.WriteRetryLater(w, httpx.TranscriptUnavailable)
		return
	}
	if rec == nil {
		httpx.WriteRecordingRefusal(w, http.StatusNotFound)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data":      rec,
		"pageCount": (rec.TranscriptCount + uint64(page.Size) - 1) / uint64(page.Size),
	})
}

// The changes to what an event's call made (seams for tests).
var (
	deleteEventArtefact  = callStateBusiness.DeleteEventArtefact
	restoreEventArtefact = callStateBusiness.RestoreEventArtefact
)

// DeleteCalendarEventCallArtefact deletes what an event's call made under
// this key, its recording and transcript (meet-02), for whoever may change
// the event by the calendar's own rule, as its call is given and taken off.
// The delete is soft: the event's "Recordings & notes" and its player stop
// showing it at once, and its Undo is the restore below. A guest who may only
// read the event is refused as for any change, anyone else is told there is
// no such event, and a key that is no recording of this event's is not found.
// DELETE /calendar/events/{eventId}/call/artefacts/{key}
func DeleteCalendarEventCallArtefact(w http.ResponseWriter, r *http.Request) {
	changeEventCallArtefact(w, r, "DeleteCalendarEventCallArtefact", deleteEventArtefact,
		"The call's recording and transcript were deleted.",
		"There's no such recording on this event.")
}

// RestoreCalendarEventCallArtefact brings back what
// DeleteCalendarEventCallArtefact deleted (its Undo), for the same people.
// POST /calendar/events/{eventId}/call/artefacts/{key}/restore
func RestoreCalendarEventCallArtefact(w http.ResponseWriter, r *http.Request) {
	changeEventCallArtefact(w, r, "RestoreCalendarEventCallArtefact", restoreEventArtefact,
		"The call's recording and transcript are back.",
		"There's nothing to bring back: it's back already, or it was never deleted.")
}

// changeEventCallArtefact is both: the calendar's change rule, then the
// change, answered in its own words (done, or missing when there was nothing
// to change).
func changeEventCallArtefact(w http.ResponseWriter, r *http.Request, where string,
	change func(context.Context, business.CallKeys, string) (bool, error), done, missing string) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	keys, err := business.Events.CallArtefactKeysToChange(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, where, err)
		return
	}
	found, err := change(r.Context(), keys, chi.URLParam(r, "key"))
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/%s: %+v", where, err)
		helpers.WriteRetryLater(w, "The recording couldn't be changed just now.")
		return
	}
	if !found {
		helpers.WriteNotFound(w, missing)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": done})
}

// SetCalendarEventCallLobby makes an event's invitees knock at its call like
// anyone from outside, or come in by their own invitation again (meet-02).
// For whoever may change the event.
// POST /calendar/events/{eventId}/call/lobby {"invitees_wait": bool}
func SetCalendarEventCallLobby(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var body struct {
		InviteesWait *bool `json:"invitees_wait"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.InviteesWait == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	call, err := business.Events.SetCallLobby(r.Context(), who, chi.URLParam(r, "eventId"), *body.InviteesWait)
	if err != nil {
		writeEventCallError(w, r, "SetCalendarEventCallLobby", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": call})
}

// writeEventCallError answers an event call's refusals: the workspace's
// switches in words, then as any calendar error.
func writeEventCallError(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "guests_off", "msg": "Guest access is off for this workspace, so events can't have calls of their own."})
	case errors.Is(err, guestBusiness.ErrRoomsOff):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "rooms_off", "msg": "Meeting rooms are off for this workspace, so events can't have calls of their own."})
	case errors.Is(err, business.ErrNoEventCall):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, business.ErrNoEventCalls):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": err.Error()})
	default:
		writeCalendarError(w, r, where, err)
	}
}
