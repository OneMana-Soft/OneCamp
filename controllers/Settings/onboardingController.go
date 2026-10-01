package controllers

// Admin: what this workspace still needs before it is useful.
//
// Reads the checklist, lets an admin dismiss it, and lets them set aside a step
// that does not apply to this workspace. Everything reported is
// derived from the live workspace, so there is nothing to keep in sync and no
// state to get wrong; see business/Onboarding for why that matters.

import (
	"encoding/json"
	"net/http"
	"strings"

	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// GetOnboardingStatus handles GET /admin/onboarding
func GetOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": onboardingBusiness.Status(ctx, userInfo),
	})
}

// DismissOnboarding handles POST /admin/onboarding/dismiss
//
// Workspace-wide and permanent. There is no undo, deliberately: an admin who
// dismisses a setup checklist has decided they know what they are doing, and a
// list that can come back is a list they have to dismiss twice.
func DismissOnboarding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := onboardingBusiness.Dismiss(); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DismissOnboarding err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Could not hide the setup checklist",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "hidden"})
}

// SkipOnboardingStep handles POST /admin/onboarding/skip.
//
// Sets one step aside, or puts it back, which is the same edit in two directions
// and therefore one endpoint with a boolean rather than two that can drift.
//
// The business layer decides whether a step may be set aside at all, so a client
// cannot hide "invite your team" by posting its id. A refusal is a 400 with the
// reason, not a 500: it is the caller that is wrong.
func SkipOnboardingStep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		ID      string `json:"id"`
		Skipped bool   `json:"skipped"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	if err := onboardingBusiness.SetStepSkipped(strings.TrimSpace(body.ID), body.Skipped); err != nil {
		helpers.LogWarnWithContext(ctx, "controllers/SkipOnboardingStep refused: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "saved"})
}
