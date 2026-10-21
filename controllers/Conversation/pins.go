package controllers

import (
	"net/http"

	pinBusiness "github.com/akashc777/OneCamp/business/Pin"
	"github.com/akashc777/OneCamp/helpers"
)

// GetPins answers a conversation's pinned messages, newest first, and whether
// the asker may pin and unpin there (chat B25).
func GetPins(w http.ResponseWriter, r *http.Request) {
	pins, err := pinBusiness.List(r.Context(), who(r), refOf(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetPins")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": pins})
}

// SetPin pins a message to its conversation, or unpins it, and answers the
// conversation's pins as they are now.
func SetPin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MessageID string `json:"message_id"`
		Pinned    bool   `json:"pinned"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx, me, ref := r.Context(), who(r), refOf(r)
	if err := pinBusiness.Set(ctx, me, ref, in.MessageID, in.Pinned); err != nil {
		answerFailure(w, r, err, "controllers/SetPin")
		return
	}
	pins, err := pinBusiness.List(ctx, me, ref)
	if err != nil {
		answerFailure(w, r, err, "controllers/SetPin")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": pins})
}
