package boardController

// Deleted boards (UX-TB-H2-BE, TABLES-BOARDS-13): the boards the caller may
// bring back, one of them, and bringing it back (business/Board boardTrash.go
// decides who may). A board that isn't one of theirs to restore answers 404,
// as a missing board does.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	business "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/controllers/httpx"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// GetBoardTrash GET /board/trash?limit=&cursor=: a page of the caller's
// Deleted boards, newest deletion first.
func GetBoardTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	isAdmin := userDomain.IsWorkspaceAdmin(ctx, userInfo.UserDgraphInfo.Uuid)
	page, err := business.BoardTrash(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, isAdmin, q.Get("cursor"), helpers.SizeParam(q, "limit", 50, 100))
	if errors.Is(err, business.ErrBoardTrashCursor) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That page isn't one the list offered. Read it again from the start."})
		return
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "deleted boards", "controllers/GetBoardTrash")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": page})
}

// GetTrashedBoard GET /board/trash/{board_uuid}: one of the caller's Deleted
// boards (a link to a deleted board offers "Restore this board"), or 404.
func GetTrashedBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	boardUUID := chi.URLParam(r, "board_uuid")
	if _, err := uuid.Parse(boardUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a board's id."})
		return
	}
	isAdmin := userDomain.IsWorkspaceAdmin(ctx, userInfo.UserDgraphInfo.Uuid)
	board, err := business.TrashedBoardFor(ctx, boardUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, isAdmin)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "board", "controllers/GetTrashedBoard")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": board})
}

// RestoreBoard POST /board/restore {board_uuid}: a deleted board back, for its
// maker or a workspace admin (not a private board's admin). 404 for anyone
// else and for a board that isn't there; 409 for one back already.
func RestoreBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var input adapter.InputUpdateBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a board's id."})
		return
	}
	isAdmin := userDomain.IsWorkspaceAdmin(ctx, userInfo.UserDgraphInfo.Uuid)
	board, err := business.RestoreBoard(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, isAdmin)
	switch {
	case errors.Is(err, helpers.ErrNotFound):
		helpers.WriteReadFailure(w, r, err, "board", "controllers/RestoreBoard")
		return
	case err != nil:
		helpers.WriteWriteFailure(w, r, err, "Couldn't bring the board back.", "controllers/RestoreBoard")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Restored", "data": board})
}
