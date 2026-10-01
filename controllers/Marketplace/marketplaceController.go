package controllers

// HTTP handlers for shareable templates: browse, publish, and install
// templates (agents, workflows, tables). Mounted under the authenticated
// member group. Install runs as the current user and reuses the app's Create
// business functions, so it can never exceed the user's own permissions (and
// re-checks capabilities for agent/workflow kinds).

import (
	"encoding/json"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/Marketplace"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func currentUser(r *http.Request) userModels.UserInfo {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return userInfo
}

func mapErr(w http.ResponseWriter, err error) {
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have permission to install this here"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "template not found"})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// ListTemplates GET /marketplace/templates?kind=
func ListTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := business.List(r.Context(), r.URL.Query().Get("kind"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetTemplate GET /marketplace/templates/{id}
func GetTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid template id"})
		return
	}
	t, gerr := business.Get(r.Context(), id)
	if gerr != nil {
		mapErr(w, gerr)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// CreateTemplate POST /marketplace/templates
func CreateTemplate(w http.ResponseWriter, r *http.Request) {
	var in business.PublishInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	t, err := business.Publish(r.Context(), in, currentUser(r).UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	auditBusiness.Record(r, "marketplace.publish", auditBusiness.CategoryApp,
		"Published template: "+t.Name, map[string]interface{}{"template_id": t.Id.String(), "kind": t.Kind})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": t})
}

// DeleteTemplate POST /marketplace/templates/{id}/delete
func DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid template id"})
		return
	}
	userInfo := currentUser(r)
	if derr := business.Unpublish(r.Context(), id, &userInfo); derr != nil {
		mapErr(w, derr)
		return
	}
	auditBusiness.Record(r, "marketplace.unpublish", auditBusiness.CategoryApp,
		"Unpublished template", map[string]interface{}{"template_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "unpublished"})
}

// InstallTemplate POST /marketplace/templates/{id}/install
func InstallTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid template id"})
		return
	}
	userInfo := currentUser(r)
	res, ierr := business.Install(r.Context(), id, &userInfo)
	if ierr != nil {
		mapErr(w, ierr)
		return
	}
	auditBusiness.Record(r, "marketplace.install", auditBusiness.CategoryApp,
		"Installed template: "+res.Name, map[string]interface{}{"template_id": id.String(), "kind": res.Kind, "entity_id": res.EntityID})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}
