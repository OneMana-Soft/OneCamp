package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/ProjectTemplate"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// Project templates: see business/ProjectTemplate. Anyone signed in lists
// them and downloads one as a file; whoever can create a project imports one;
// a project's admins save the project as one; a saved one is its author's, or
// an admin's, to delete. Starting a project from one is CreateProject's
// template_id.

// maxFile is the largest template file read: MaxTasks tasks with long
// descriptions fit.
const maxFile = 12 << 20

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var te *business.TemplateError
	switch {
	case errors.As(err, &te):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": te.Error()})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That template isn't there any more."})
	case errors.Is(err, business.ErrNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the person who saved a template, or an admin, can delete it."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectTemplate/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

func me(w http.ResponseWriter, r *http.Request) (*userModels.UserInfo, bool) {
	u, ok := userModels.FromContext(r.Context())
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
	}
	return u, ok
}

func read(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxFile)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That template couldn't be read. Is it a OneCamp template file?"})
		return false
	}
	return true
}

// ListTemplates is the built-in templates, then the saved ones, newest first.
// GET /project/templates
func ListTemplates(w http.ResponseWriter, r *http.Request) {
	u, ok := me(w, r)
	if !ok {
		return
	}
	list, err := business.List(r.Context(), u)
	if err != nil {
		write(w, r, "ListTemplates", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": list})
}

// GetTemplate is one template in full, to download.
// GET /project/templates/{template_id}
func GetTemplate(w http.ResponseWriter, r *http.Request) {
	t, err := business.Get(r.Context(), chi.URLParam(r, "template_id"))
	if err != nil {
		write(w, r, "GetTemplate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// ImportTemplate keeps a template from a file, for whoever can create a
// project: a team's admin, or a workspace admin.
// POST /project/templates
func ImportTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := me(w, r)
	if !ok {
		return
	}
	if !u.UserPostgresInfo.IsAdmin {
		teams, err := teamBusiness.GetDgraphTeamListByAdminDgraphUID(r.Context(), u.UserDgraphInfo.Uid)
		if err != nil {
			write(w, r, "ImportTemplate teams", err)
			return
		}
		if len(teams) == 0 {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a team's admins, who create its projects, can add templates."})
			return
		}
	}
	var t business.Template
	if !read(w, r, &t) {
		return
	}
	s, err := business.Save(r.Context(), t, u)
	if err != nil {
		write(w, r, "ImportTemplate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// SaveProjectAsTemplate keeps the project as it is now as a template, named
// as its admin chose. tz is their zone, which its dates are counted in.
// POST /project/{project_uuid}/save-as-template
func SaveProjectAsTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := me(w, r)
	if !ok {
		return
	}
	id, _, ok := projectaccess.Require(w, r, true, "Only the project's admins can save it as a template.")
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		TZ          string `json:"tz"`
	}
	if !read(w, r, &in) {
		return
	}
	t, err := business.Snapshot(r.Context(), id, in.Name, in.Description, helpers.Location(in.TZ))
	if err != nil {
		write(w, r, "SaveProjectAsTemplate snapshot", err)
		return
	}
	s, err := business.Save(r.Context(), t, u)
	if err != nil {
		write(w, r, "SaveProjectAsTemplate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// DeleteTemplate removes a saved template.
// POST /project/templates/{template_id}/delete
func DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := me(w, r)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), chi.URLParam(r, "template_id"), u); err != nil {
		write(w, r, "DeleteTemplate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted."})
}
