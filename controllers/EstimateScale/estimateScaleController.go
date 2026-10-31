// Package controllers is a project's estimate scale (linear-05): its people
// read it, its admins set it. See business/EstimateScale.
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// GetScale is the project's scale (null: it estimates in hours), the scales
// it may take, and whether the caller may change it, for anyone who may see
// the project. GET /project/{project_uuid}/estimates
func GetScale(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	answer, err := Answer(r.Context(), id, standing)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/EstimateScale/GetScale err: %+v", err)
		helpers.WriteRetryLater(w, "The project's estimates couldn't be read just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
}

// Answer is what GET /project/{p}/estimates answers a reader with this
// standing in the project: its scale (null: hours), the scales it may take,
// and whether the reader may change it. The task panel's include asks for it
// too (controllers/Task), so a task's Points row paints with the task.
func Answer(ctx context.Context, project uuid.UUID, standing workAccess.Standing) (map[string]any, error) {
	s, err := estimateScale.Of(ctx, project)
	if err != nil {
		return nil, err
	}
	return map[string]any{"scale": estimateScale.ViewOf(s), "kinds": estimateScale.Kinds, "can_edit": standing.Can(workAccess.NeedManage)}, nil
}

// SetScale puts the project on a scale, or back on hours (kind "hours"), for
// its admins. POST /project/{project_uuid}/estimates
// {kind, extended, allow_zero, count_unestimated}
func SetScale(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins can change how it estimates.")
	if !ok {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in estimateScale.Scale
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	s, err := estimateScale.Set(r.Context(), id, in, u.UserPostgresInfo.Id)
	switch {
	case errors.Is(err, estimateScale.ErrUnknownKind):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A project estimates in hours, or in points on the exponential, Fibonacci, linear or T-shirt scale."})
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/EstimateScale/SetScale err: %+v", err)
		helpers.WriteRetryLater(w, "The project's estimates couldn't be saved just now.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved", "data": map[string]any{"scale": estimateScale.ViewOf(s)}})
	}
}
