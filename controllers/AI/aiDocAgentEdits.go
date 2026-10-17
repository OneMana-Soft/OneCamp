package controllers

// An agent's edits in a doc (docs programme B9, editor.md P2-2):
//
//	GET  /doc/ai/agent-edits?doc_uuid=   the doc's agent edits, newest first, for
//	                                     the margin chips ("Release Captain · 2m ·
//	                                     Revert"), for anyone who can read it
//	POST /doc/ai/agent-edits/revert      {doc_uuid, run_id | edit_id, force}:
//	                                     put back what was there before
//
// A revert of text people have edited since answers 409 with edited_since, and
// the web app asks before sending force. The rules are business/AI docEdit.go.

import (
	"encoding/json"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// ListDocAgentEdits handles GET /doc/ai/agent-edits.
func ListDocAgentEdits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	edits, err := business.ListAgentEdits(ctx, &userInfo, r.URL.Query().Get("doc_uuid"))
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/ListDocAgentEdits: %v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The doc's agent edits couldn't be read just now."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": edits})
}

// RevertDocAgentEdits handles POST /doc/ai/agent-edits/revert.
func RevertDocAgentEdits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var req struct {
		DocUUID string `json:"doc_uuid"`
		RunID   string `json:"run_id"`
		EditID  string `json:"edit_id"`
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The request couldn't be read."})
		return
	}
	if !limitedDocAI(w, r, userInfo.UserDgraphInfo.Uuid) {
		return
	}
	n, err := business.RevertAgentEdits(ctx, &userInfo, req.DocUUID, req.RunID, req.EditID, req.Force)
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		var since *business.EditedSinceError
		var invalid *business.DocAIInvalidError
		var cant *business.RevertError
		switch {
		case errors.As(err, &since):
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": since.Error(), "edited_since": true, "reverted": n})
		case errors.As(err, &invalid):
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Msg})
		case errors.As(err, &cant):
			helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": cant.Msg, "reverted": n})
		default:
			helpers.LogErrorWithContext(ctx, "controllers/RevertDocAgentEdits: %v", err)
			helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The revert couldn't be made just now. Try again in a moment.", "reverted": n})
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Reverted.", "data": map[string]int{"reverted": n}})
}
