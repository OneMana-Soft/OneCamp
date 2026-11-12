package controllers

// HTTP handlers for AI Proactive Nudges.
//
// User-facing endpoints (mounted under /ai, any authenticated user) are always
// scoped to the caller — a user can only see and mutate their OWN nudges. The
// enable toggle is admin-only (mounted under /admin/ai).

import (
	"encoding/json"
	"net/http"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxNudgesReturned bounds the list endpoint.
const maxNudgesReturned = 50

// ListNudges handles GET /ai/nudges — the caller's open nudges, newest/highest
// priority first, each marked unread as the Inbox badge counts it, plus the
// open count.
func ListNudges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	items, err := nudgeModels.ListOpenForUser(ctx, userInfo.UserPostgresInfo.Id, maxNudgesReturned)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListNudges failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load nudges"})
		return
	}
	// Guarantee a non-nil slice so the JSON is `[]`, not `null` — a null list
	// would crash the FE's globally-mounted nudge bell (`nudges.length`).
	if items == nil {
		items = []*nudgeModels.Nudge{}
	}
	// Each one's dot as the Inbox badge counts it. No dots rather than a
	// guess when their last look can't be read.
	if err := aiBusiness.MarkNudgesUnread(ctx, userInfo.UserPostgresInfo.Id, items); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListNudges: %s's nudges couldn't be marked: %+v", userInfo.UserPostgresInfo.Id, err)
	}
	count, _ := nudgeModels.CountOpenForUser(ctx, userInfo.UserPostgresInfo.Id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]interface{}{"nudges": items, "open_count": count},
	})
}

// DismissNudge handles POST /ai/nudges/{id}/dismiss — mark one nudge dismissed.
func DismissNudge(w http.ResponseWriter, r *http.Request) {
	setNudgeStatus(w, r, nudgeModels.StatusDismissed)
}

// ActNudge handles POST /ai/nudges/{id}/act — mark one nudge acted-on (the user
// followed its CTA). Distinguished from dismiss so we can suppress re-surfacing
// a handled signal and, later, measure nudge usefulness.
func ActNudge(w http.ResponseWriter, r *http.Request) {
	setNudgeStatus(w, r, nudgeModels.StatusActed)
}

// setNudgeStatus is the shared transition handler for dismiss/act. The model's
// user_id predicate is the authorization check.
func setNudgeStatus(w http.ResponseWriter, r *http.Request, status string) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid nudge id"})
		return
	}

	uid := userInfo.UserPostgresInfo.Id
	if err := nudgeModels.SetStatus(ctx, id, uid, status); err != nil {
		code := http.StatusBadRequest
		if err.Error() == "nudge not found" {
			code = http.StatusNotFound
		}
		helpers.WriteJSON(w, code, helpers.Envolope{"msg": err.Error()})
		return
	}

	// Their other open tabs drop this nudge, by its id, at once.
	count, _ := nudgeModels.CountOpenForUser(ctx, uid)
	aiBusiness.PublishNudgeCleared(uid.String(), count, id)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]int{"open_count": count}})
}

// DismissAllNudges handles POST /ai/nudges/dismiss-all — "mark all read".
func DismissAllNudges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	uid := userInfo.UserPostgresInfo.Id
	n, err := nudgeModels.DismissAllForUser(ctx, uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DismissAllNudges failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to dismiss nudges"})
		return
	}
	aiBusiness.PublishNudgeCleared(uid.String(), 0)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]int64{"dismissed": n}})
}

// SetNudges handles POST /admin/ai/nudges (admin toggle for the engine).
func SetNudges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest // reuse {enabled bool}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := aiBusiness.SetNudgesEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	recordAIChange(r, "ai.nudges.set", "Turned AI nudges on or off", aiMeta{"enabled": req.Enabled})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "nudges toggled"})
}
