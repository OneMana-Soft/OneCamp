package controllers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/helpers"
)

// BoardCalls answers a board's calls (miro-27, calls with the board): GET
// /board/getBoardCalls/{board_uuid}, the calls that showed it, the latest
// first, with what each kept, as business/CallState BoardCalls lets this
// person see them. A board they can't read is 404; an API token or a bot is
// refused.
func BoardCalls(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	calls, err := callStateBusiness.BoardCalls(r.Context(), &userInfo, chi.URLParam(r, "board_uuid"))
	switch {
	case errors.Is(err, callStateBusiness.ErrBoardCallsRefused):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "A board's calls are read in the app, as yourself."})
	case errors.Is(err, callStateBusiness.ErrBoardNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no board here you can see."})
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/BoardCalls failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"calls": calls}})
	}
}
