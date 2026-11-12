package boardController

// Presenter notes (miro-28 2a): business/Board notes.go has who may read and
// write them. Both routes sit behind the member gate, so a share-link guest,
// a call's guest and the public never reach them, nor does an API token.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	business "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

// GetFrameNotes GET /board/getFrameNotes/{board_uuid}: the board's presenter
// notes, for the frames on it, to who may present it.
func GetFrameNotes(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	user := userInfo.UserDgraphInfo
	notes, err := business.FrameNotes(r.Context(), chi.URLParam(r, "board_uuid"), user.Uid, user.Uuid)
	if err != nil {
		writeNotesFailure(w, r, err, "controllers/GetFrameNotes")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": notes})
}

// SaveFrameNote POST /board/saveFrameNote {board_uuid, frame_id, body,
// version}: writes a frame's note on the version read (0 for none); an empty
// body deletes it. 409 stale_note carries the note as it now is.
func SaveFrameNote(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in struct {
		BoardUUID string `json:"board_uuid"`
		FrameID   string `json:"frame_id"`
		Body      string `json:"body"`
		Version   int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteBadRequest(w, "Send the board's id, the frame's id, the note and the version you read.")
		return
	}
	user := userInfo.UserDgraphInfo
	note, err := business.SaveFrameNote(r.Context(), in.BoardUUID, in.FrameID, in.Body, in.Version, user.Uid, user.Uuid)
	var stale *business.StaleNoteError
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": note})
	case errors.As(err, &stale):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "stale_note", "msg": "Someone else changed these notes.", "data": stale.Current})
	case errors.Is(err, business.ErrNoteTooLong):
		helpers.WriteBadRequest(w, fmt.Sprintf("A note is at most %d characters.", business.NoteMaxChars))
	case errors.Is(err, business.ErrBadFrame):
		helpers.WriteBadRequest(w, "Send the frame's id and the note as text.")
	default:
		writeNotesFailure(w, r, err, "controllers/SaveFrameNote")
	}
}

// writeNotesFailure answers a notes read or write that was refused or failed.
func writeNotesFailure(w http.ResponseWriter, r *http.Request, err error, where string) {
	switch {
	case errors.Is(err, business.ErrBoardNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Board not found"})
	case errors.Is(err, business.ErrNotThePresenter):
		helpers.WriteForbidden(w, "Only people who can present this board read its notes.")
	case errors.Is(err, business.ErrCannotEditNotes):
		helpers.WriteForbidden(w, "Only people who can edit this board write its notes.")
	default:
		helpers.LogErrorWithContext(r.Context(), "%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't reach the notes. Try again."})
	}
}
