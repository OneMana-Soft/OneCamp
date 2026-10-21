package controllers

// Admin: what this workspace still needs before it is useful.
//
// Reads the checklist, lets an admin dismiss it, and lets them set aside a step
// that does not apply to this workspace. Everything reported is
// derived from the live workspace, so there is nothing to keep in sync and no
// state to get wrong; see business/Onboarding for why that matters.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

// GetOnboardingStatus handles GET /admin/onboarding
func GetOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

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

// MakeOnboardingSample handles POST /admin/onboarding/sample: the sample
// project, made once from the built-in client project template, as the admin.
func MakeOnboardingSample(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	sample, err := onboardingBusiness.MakeSample(ctx, userInfo)
	if err != nil && sample.ProjectUUID == "" {
		helpers.LogErrorWithContext(ctx, "controllers/MakeOnboardingSample err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't make the sample project. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sample})
}

// RemoveOnboardingSample handles POST /admin/onboarding/sample/remove:
// archives what the sample made, and nothing else.
func RemoveOnboardingSample(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	switch err := onboardingBusiness.RemoveSample(ctx, userInfo); {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "removed"})
	case errors.Is(err, onboardingBusiness.ErrNoSample):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no sample project to remove."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/RemoveOnboardingSample err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't remove the sample project. Try again in a moment."})
	}
}
