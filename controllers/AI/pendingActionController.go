package controllers

// HTTP surface for durable in-thread AI write approvals.
//
// The bot/assistant proposes a write; it is persisted as a durable record and
// surfaced as an in-thread Approve/Deny card. These handlers let the requester
// list their open approvals (reconcile-on-load) and approve/reject one. Approve
// executes the action server-side, at most once, AS the approver, with that
// user's permissions re-checked (no confused-deputy). Routes are GET/POST only.

import (
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ListPendingActions handles GET /ai/pending-actions
// Returns the caller's open (pending, unexpired) write approvals, newest first.
func ListPendingActions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	uid, err := uuid.Parse(userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid user"})
		return
	}

	actions, err := business.ListOpenPendingActions(ctx, uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListPendingActions err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load pending actions"})
		return
	}
	if actions == nil {
		actions = []*pendingModels.PendingAction{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": actions})
}

// ApprovePendingAction handles POST /ai/pending-actions/{id}/approve
// Approves a proposed write; it executes at most once, as the approver.
func ApprovePendingAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid action id"})
		return
	}

	action, err := business.ApprovePendingAction(ctx, &userInfo, id)
	if err != nil {
		status, msg := pendingActionStatus(err)
		// On a "resolved/expired" race the latest record is still returned so
		// the client can reconcile its card to the authoritative state.
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg, "data": action})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Action approved", "data": action})
}

// RejectPendingAction handles POST /ai/pending-actions/{id}/reject
// Denies a proposed write. Idempotent on an already-resolved action.
func RejectPendingAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid action id"})
		return
	}

	action, err := business.RejectPendingAction(ctx, &userInfo, id)
	if err != nil {
		status, msg := pendingActionStatus(err)
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg, "data": action})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Action rejected", "data": action})
}

// pendingActionStatus maps a business error to an HTTP status + message.
func pendingActionStatus(err error) (int, string) {
	switch {
	case errors.Is(err, business.ErrPendingActionNotFound):
		return http.StatusNotFound, "Pending action not found"
	case errors.Is(err, business.ErrPendingActionForbidden):
		return http.StatusForbidden, "Not authorized to act on this action"
	case errors.Is(err, business.ErrPendingActionResolved):
		return http.StatusConflict, "This action was already handled"
	case errors.Is(err, business.ErrPendingActionExpired):
		return http.StatusGone, "This action expired"
	default:
		return http.StatusInternalServerError, "Failed to process action"
	}
}
