package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	checkInBusiness "github.com/akashc777/OneCamp/business/CheckIn"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Check-ins: see business/CheckIn. A channel's members see its check-ins; its
// moderators and workspace admins set them up, change them and ask now.

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ce *checkInBusiness.CheckInError
	switch {
	case errors.As(err, &ce):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ce.Error()})
	case errors.Is(err, checkInBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That check-in or channel no longer exists."})
	case errors.Is(err, checkInBusiness.ErrNotAllowed):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the channel's moderators or a workspace admin can change its check-ins."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/CheckIn/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

func readerOf(r *http.Request) checkInBusiness.Reader {
	u := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return checkInBusiness.Reader{User: &u, IsAdmin: u.UserPostgresInfo.IsAdmin}
}

func param(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That check-in or channel no longer exists."})
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that check-in."})
		return false
	}
	return true
}

// ListCheckIns is a channel's check-ins. GET /ch/{channel_uuid}/checkins
func ListCheckIns(w http.ResponseWriter, r *http.Request) {
	ch, ok := param(w, r, "channel_uuid")
	if !ok {
		return
	}
	list, canEdit, err := checkInBusiness.List(r.Context(), readerOf(r), ch)
	if err != nil {
		write(w, r, "ListCheckIns", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"checkins": list, "can_edit": canEdit}})
}

// CreateCheckIn sets one up. POST /ch/{channel_uuid}/checkins {question, days, time, tz}
func CreateCheckIn(w http.ResponseWriter, r *http.Request) {
	ch, ok := param(w, r, "channel_uuid")
	if !ok {
		return
	}
	var in checkInBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	v, err := checkInBusiness.Create(r.Context(), readerOf(r), ch, in, time.Now())
	if err != nil {
		write(w, r, "CreateCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// EditCheckIn changes one. POST /ch/checkins/{checkin_id}/edit {question, days, time, tz}
func EditCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := param(w, r, "checkin_id")
	if !ok {
		return
	}
	var in checkInBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	v, err := checkInBusiness.Edit(r.Context(), readerOf(r), id, in, time.Now())
	if err != nil {
		write(w, r, "EditCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// PauseCheckIn pauses or resumes one. POST /ch/checkins/{checkin_id}/pause {paused}
func PauseCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := param(w, r, "checkin_id")
	if !ok {
		return
	}
	var in struct {
		Paused bool `json:"paused"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := checkInBusiness.SetPaused(r.Context(), readerOf(r), id, in.Paused, time.Now())
	if err != nil {
		write(w, r, "PauseCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// DeleteCheckIn removes one; its past questions stay in the channel. POST /ch/checkins/{checkin_id}/delete
func DeleteCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := param(w, r, "checkin_id")
	if !ok {
		return
	}
	if err := checkInBusiness.Delete(r.Context(), readerOf(r), id); err != nil {
		write(w, r, "DeleteCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Check-in deleted."})
}

// AskCheckInNow asks the question at once. POST /ch/checkins/{checkin_id}/ask
func AskCheckInNow(w http.ResponseWriter, r *http.Request) {
	id, ok := param(w, r, "checkin_id")
	if !ok {
		return
	}
	post, err := checkInBusiness.AskNow(r.Context(), readerOf(r), id, time.Now())
	if err != nil {
		write(w, r, "AskCheckInNow", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"post_uuid": post}})
}
