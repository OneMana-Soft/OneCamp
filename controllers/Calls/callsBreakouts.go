package controllers

// A call's breakout rooms' tokens (calls package C):
//   - POST /calls/{session_id}/breakout-token {room, language}: a member's, by
//     their session, for a room of the call whose own session that is;
//   - POST /guest/calls/breakout-token {room, language}: a guest's, proved by
//     their own call token (Authorization: Bearer <token>).
// Running the rooms is POST /calls/{session_id}/act (callsController.go).

import (
	"encoding/json"
	"errors"
	"net/http"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// breakoutAsk is the room someone asks a token for, from 1, and the language they
// speak (their browser's), for the server's transcriber.
type breakoutAsk struct {
	Room     int    `json:"room"`
	Language string `json:"language"`
}

// CallBreakoutToken POST /calls/{session_id}/breakout-token.
func CallBreakoutToken(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	var ask breakoutAsk
	if err := json.NewDecoder(r.Body).Decode(&ask); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	token, err := callControlBusiness.BreakoutToken(r.Context(), &userInfo.UserDgraphInfo, chi.URLParam(r, "session_id"), ask.Room, ask.Language)
	writeRoomToken(w, r, token, err)
}

// GuestCallBreakoutToken POST /guest/calls/breakout-token.
func GuestCallBreakoutToken(w http.ResponseWriter, r *http.Request) {
	var ask breakoutAsk
	if err := json.NewDecoder(r.Body).Decode(&ask); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	token, err := callControlBusiness.GuestBreakoutToken(r.Context(), r.Header.Get("Authorization"), ask.Room, ask.Language)
	writeRoomToken(w, r, token, err)
}

// writeRoomToken answers a room's token, or why not.
func writeRoomToken(w http.ResponseWriter, r *http.Request, token callControlBusiness.RoomToken, err error) {
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls a breakout room's token failed: %+v", err)
		helpers.WriteRetryLater(w, "The call server couldn't open that room just now. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": token})
}
