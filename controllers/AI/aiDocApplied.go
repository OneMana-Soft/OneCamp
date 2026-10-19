package controllers

// Doc AI's record and the empty doc's offer (docs programme B9):
//
//	POST /doc/ai/applied    an accepted AI edit, recorded (ai-search-io P1-2)
//	GET  /doc/ai/activity   "AI activity on this doc", for anyone who can read it
//	GET  /doc/ai/starters   what an empty doc's AI row offers its editor
//
// All of it is business/AI docAIRecord.go and docAIStarters.go; this file only
// reads the request and says the answer in words.

import (
	"encoding/json"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// writeDocAccessError answers a doc refusal; ok is false for any other error.
func writeDocAccessError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, business.ErrDocAINotFound):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, business.ErrDocAICantEdit):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can view this doc but not edit it."})
	default:
		return false
	}
	return true
}

// limitedDocAI answers 429 when the person has asked doc AI too much in the
// last minute; ok is false when it answered. Every route that writes to the
// record goes through it, as the stream does.
func limitedDocAI(w http.ResponseWriter, r *http.Request, userUUID string) bool {
	if err := docAIRun.CheckRateLimit(r.Context(), userUUID); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "You've asked AI a lot in the last minute. Try again in a moment."})
		return false
	}
	return true
}

// recordDocAIEdit records an accepted edit: a seam for tests.
var recordDocAIEdit = business.RecordDocAIEdit

// RecordDocAIApplied handles POST /doc/ai/applied.
func RecordDocAIApplied(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in business.DocAIEdit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The request couldn't be read."})
		return
	}
	if !limitedDocAI(w, r, userInfo.UserDgraphInfo.Uuid) {
		return
	}
	// Which model wrote it, and where it runs, is the server's to say: the
	// person's model as the server resolves it, never what the client claims.
	in.Model, in.Local = "", false
	if docAIRun.Enabled() {
		_, _, _, model := docAIRun.Resolve(ctx, userInfo.UserDgraphInfo.Uuid)
		in.Model, in.Local = model.Label, model.Local
	}
	if err := recordDocAIEdit(ctx, &userInfo, in); err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		var invalid *business.DocAIInvalidError
		if errors.As(err, &invalid) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Msg})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/RecordDocAIApplied: %v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The edit is in the doc, but it couldn't be recorded just now."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Recorded.", "data": map[string]bool{"recorded": true}})
}

// DocAIActivity handles GET /doc/ai/activity?doc_uuid=&limit=.
func DocAIActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !limitedDocAI(w, r, userInfo.UserDgraphInfo.Uuid) {
		return
	}
	limit := helpers.LimitParam(r.URL.Query(), 50, 100)
	items, err := business.DocAIActivity(ctx, &userInfo, r.URL.Query().Get("doc_uuid"), limit)
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/DocAIActivity: %v", err)
		helpers.WriteRetryLater(w, "The doc's AI activity couldn't be read just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": items})
}

// DocAIStarters handles GET /doc/ai/starters?doc_uuid=.
func DocAIStarters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	res, err := business.DocAIStarters(ctx, &userInfo, r.URL.Query().Get("doc_uuid"))
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/DocAIStarters: %v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't check the doc's project just now."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": res})
}
