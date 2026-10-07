package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AI"
	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/go-chi/chi/v5"
)

// A project's plan drafted by the AI from a sentence, for New project: see
// business/AI/projectPlanDraft.go. Starting a draft answers at once; the app
// then asks for it until it's done. For whoever can create a project.

const planTryAgain = "The plan couldn't be drafted just now. Try again, or pick a template."

// StartProjectPlan handles POST /ai/project-template/draft {description}.
func StartProjectPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userModels.FromContext(ctx)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return
	}
	may, err := templateBusiness.MayCreateProjects(ctx, user)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AI/StartProjectPlan teams err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": planTryAgain})
		return
	}
	if !may {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a team's admins, who create its projects, can draft one."})
		return
	}
	var in struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Describe the project in a sentence."})
		return
	}
	d, err := business.StartProjectPlan(ctx, user, in.Description)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
	case errors.Is(err, business.ErrPlanTooShort):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say a little more: what the project is for, and by when."})
	case errors.Is(err, ai.ErrRateLimited):
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "That's a lot of AI requests in a short time. Try again in a minute."})
	case errors.Is(err, ai.ErrAIDisabled):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The AI isn't on in this workspace. Pick a template instead."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/AI/StartProjectPlan err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": planTryAgain})
	}
}

// ProjectPlan handles GET /ai/project-template/draft/{draft_id}: the draft as
// it stands, drafting, done (with the plan) or failed (with why).
func ProjectPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userModels.FromContext(ctx)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return
	}
	d, err := business.ProjectPlan(ctx, user.UserDgraphInfo.Uuid, chi.URLParam(r, "draft_id"))
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
	case errors.Is(err, business.ErrPlanGone):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That draft isn't there any more. Describe the project again."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/AI/ProjectPlan err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": planTryAgain})
	}
}
