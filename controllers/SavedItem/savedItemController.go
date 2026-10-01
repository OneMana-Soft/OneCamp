// Package controller (SavedItem) serves Save for later. Every handler acts on
// the signed-in member's own items only.
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/SavedItem"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func member(r *http.Request) (uuid.UUID, bool) {
	u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(u.UserDgraphInfo.Uuid)
	return id, err == nil
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, business.ErrInvalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That can't be saved as given. Check the reminder time is within a year."})
	case errors.Is(err, business.ErrTooMany):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "You have 1,000 things saved for later. Mark some done to save more."})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That saved item no longer exists."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/SavedItem err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Could not update Later. Try again."})
	}
}

// List GET /later?state=open|done
func List(w http.ResponseWriter, r *http.Request) {
	userID, ok := member(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	res, err := business.List(r.Context(), userID, r.URL.Query().Get("state") == "done")
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// Save POST /later/save
func Save(w http.ResponseWriter, r *http.Request) {
	userID, ok := member(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	var in business.SaveInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	item, err := business.Save(r.Context(), userID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": item})
}

type updateInput struct {
	ID string `json:"id"`
	// Exactly one of these is acted on, in this order.
	Done        *bool      `json:"done,omitempty"`
	ClearRemind bool       `json:"clear_remind,omitempty"`
	RemindAt    *time.Time `json:"remind_at,omitempty"`
}

// Update POST /later/update: finish or reopen an item, or change its reminder.
func Update(w http.ResponseWriter, r *http.Request) {
	userID, ok := member(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	var in updateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	var item any
	switch {
	case in.Done != nil:
		item, err = business.SetDone(r.Context(), userID, id, *in.Done)
	case in.ClearRemind:
		item, err = business.SetReminder(r.Context(), userID, id, nil)
	case in.RemindAt != nil:
		item, err = business.SetReminder(r.Context(), userID, id, in.RemindAt)
	default:
		err = business.ErrInvalid
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": item})
}

// Delete POST /later/delete
func Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := member(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	if err := business.Delete(r.Context(), userID, id); err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed"})
}
