package controllers

// Where you are in a conversation, and Mark unread from a message (chat A3):
// GET /conversations/{kind}/{id}/read-state and POST .../unread {from}. Both
// answer 404 alike for a conversation you can't read and one that isn't there.

import (
	"net/http"

	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	"github.com/akashc777/OneCamp/helpers"
)

// GetReadState is where the reader is in the conversation: unread, the first
// unread message and since when.
func GetReadState(w http.ResponseWriter, r *http.Request) {
	p, err := readStateBusiness.States.Position(r.Context(), who(r), refOf(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetReadState")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}

// MarkUnread makes the message named, and what follows it, unread for the
// reader, and answers where they now are.
func MarkUnread(w http.ResponseWriter, r *http.Request) {
	var in struct {
		From string `json:"from"`
	}
	if !decode(w, r, &in) {
		return
	}
	ref := refOf(r)
	if err := readStateBusiness.States.MarkUnread(r.Context(), who(r), ref, in.From); err != nil {
		answerFailure(w, r, err, "controllers/MarkUnread")
		return
	}
	p, err := readStateBusiness.States.Position(r.Context(), who(r), ref)
	if err != nil {
		answerFailure(w, r, err, "controllers/MarkUnread")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}
