// Package controller (ScheduledMessage) serves "send later" for channels,
// direct messages and groups. Every handler acts on the signed-in person's own
// scheduled messages only.
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/ScheduledMessage"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func person(r *http.Request) (*userModels.UserInfo, bool) {
	u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return &u, ok
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var inv *business.Invalid
	switch {
	case errors.As(err, &inv):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": inv.Msg})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That scheduled message no longer exists."})
	case errors.Is(err, business.ErrNotPending):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That message has already been sent or cancelled."})
	default:
		if rj, ok := sendBusiness.AsRejection(err); ok {
			helpers.WriteJSON(w, rj.Status, helpers.Envolope{"msg": rj.Msg})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/ScheduledMessage err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't update your scheduled messages. Try again."})
	}
}

type scheduleInput struct {
	Kind   string          `json:"kind"`
	Body   json.RawMessage `json:"body"`
	SendAt time.Time       `json:"send_at"`
}

// Schedule POST /message/schedule
func Schedule(w http.ResponseWriter, r *http.Request) {
	u, ok := person(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	var in scheduleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	v, err := business.Schedule(r.Context(), u, in.Kind, in.Body, in.SendAt)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Scheduled", "data": v})
}

// List GET /message/scheduled?target=<conversation id>
func List(w http.ResponseWriter, r *http.Request) {
	u, ok := person(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	vs, err := business.List(r.Context(), u, r.URL.Query().Get("target"))
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": vs})
}

type idInput struct {
	ID     string          `json:"id"`
	SendAt time.Time       `json:"send_at"`
	Body   json.RawMessage `json:"body,omitempty"`
}

func readID(w http.ResponseWriter, r *http.Request) (*userModels.UserInfo, idInput, uuid.UUID, bool) {
	var in idInput
	u, ok := person(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return nil, in, uuid.Nil, false
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return nil, in, uuid.Nil, false
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a scheduled message."})
		return nil, in, uuid.Nil, false
	}
	return u, in, id, true
}

// Update POST /message/scheduled/update {id, send_at, body?}
func Update(w http.ResponseWriter, r *http.Request) {
	u, in, id, ok := readID(w, r)
	if !ok {
		return
	}
	v, err := business.Update(r.Context(), u, id, in.SendAt, in.Body)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated", "data": v})
}

// Cancel POST /message/scheduled/cancel {id}
func Cancel(w http.ResponseWriter, r *http.Request) {
	u, _, id, ok := readID(w, r)
	if !ok {
		return
	}
	if err := business.Cancel(r.Context(), u, id); err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Cancelled"})
}

// SendNow POST /message/scheduled/sendNow {id}
func SendNow(w http.ResponseWriter, r *http.Request) {
	u, _, id, ok := readID(w, r)
	if !ok {
		return
	}
	if err := business.SendNow(r.Context(), u, id); err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
}
