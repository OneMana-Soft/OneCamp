package controllers

// Polls in channels. Every handler acts as the signed-in person and is held to
// that person's channel membership, the same as reading or posting there.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

func currentUser(w http.ResponseWriter, r *http.Request) (*userModels.UserInfo, bool) {
	info, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteUnauthorized(w)
		return nil, false
	}
	return &info, true
}

// fail answers a failed poll call: what the person can fix is a 400, a poll
// they cannot see is a 404 either way, anything else is ours and logged.
func fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	var input pollBusiness.InputError
	switch {
	case errors.As(err, &input):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": input.Error()})
	case errors.Is(err, pollBusiness.ErrNoAccess):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, pollBusiness.ErrNotBoardEditor):
		helpers.WriteForbidden(w, "Only people who can edit this board start or close its polls.")
	case errors.Is(err, pollBusiness.ErrFrameNotOnBoard):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "frame_not_on_board",
			"msg": "That frame isn't on this board yet. A frame drawn just now is saved within half a minute: try again then."})
	case errors.Is(err, boardBusiness.ErrDocumentUnreadable):
		helpers.WriteRetryLater(w, "Couldn't read the board just now. Try again.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/poll %s: %v", what, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Something went wrong with the poll. Try again."})
	}
}

// CreatePoll handles POST /poll {channel_uuid, question, options[], multiple, open_hours}.
func CreatePoll(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	var in struct {
		ChannelUUID string   `json:"channel_uuid"`
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		Multiple    bool     `json:"multiple"`
		OpenHours   int      `json:"open_hours"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid body"})
		return
	}
	v, err := pollBusiness.Create(r.Context(), user, pollBusiness.NewPoll{
		ChannelUUID: in.ChannelUUID, Question: in.Question, Options: in.Options, Multiple: in.Multiple, OpenHours: in.OpenHours,
	})
	if err != nil {
		fail(w, r, err, "create")
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": v})
}

// GetPoll handles GET /poll/{id}.
func GetPoll(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	v, err := pollBusiness.Get(r.Context(), user, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, r, err, "get")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// VotePoll handles POST /poll/{id}/vote {option_ids[]}; an empty list retracts.
func VotePoll(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	var in struct {
		OptionIDs []string `json:"option_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid body"})
		return
	}
	v, err := pollBusiness.Vote(r.Context(), user, chi.URLParam(r, "id"), in.OptionIDs)
	if err != nil {
		fail(w, r, err, "vote")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// ClosePoll handles POST /poll/{id}/close.
func ClosePoll(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	v, err := pollBusiness.Close(r.Context(), user, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, r, err, "close")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// CreateBoardPoll handles POST /board/polls {board_uuid, frame_id, question,
// options[], multiple}: a poll on a frame of a board, by who may edit it.
func CreateBoardPoll(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	var in pollBusiness.BoardPoll
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid body"})
		return
	}
	v, err := pollBusiness.CreateOnBoard(r.Context(), user, in)
	if err != nil {
		fail(w, r, err, "create on board")
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": v})
}

// BoardPolls handles GET /board/polls/{board_uuid}: the board's polls, oldest
// first, to whoever may open it.
func BoardPolls(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	v, err := pollBusiness.BoardViews(r.Context(), user, chi.URLParam(r, "board_uuid"))
	if err != nil {
		fail(w, r, err, "board's polls")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}
