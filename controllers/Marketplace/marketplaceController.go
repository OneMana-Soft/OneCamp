package controllers

// HTTP handlers for shareable templates: browse, publish, and install
// templates (agents, workflows, tables). Mounted under the authenticated
// member group. Install runs as the current user and reuses the app's Create
// business functions, so it can never exceed the user's own permissions (and
// re-checks capabilities for agent/workflow kinds).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/Marketplace"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// installTemplate is business.Install: a seam, so how a failed install is
// answered is tested without the stores.
var installTemplate = business.Install

func currentUser(r *http.Request) userModels.UserInfo {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return userInfo
}

// mapErr answers a failed browse, publish or unpublish: a refusal, a
// template they may not touch and one that isn't there in their words
// (answered); anything else is a 400 with err's text (publish's checks are
// plain errors in the person's words).
func mapErr(w http.ResponseWriter, err error) {
	if answered(w, err) {
		return
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
}

// answered writes a refusal (its status and words), a template they may not
// install or change here (403) and one that isn't there (404), and says
// whether it did.
func answered(w http.ResponseWriter, err error) bool {
	refusal, refused := business.AsRefusal(err)
	switch {
	case refused:
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have permission to install this here"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "template not found"})
	default:
		return false
	}
	return true
}

// installFailed answers a failed install. A refusal (the install's own, or
// what the kind's create refused: business.AsRefusal), a forbidden and a
// missing template keep their status and words (answered); anything else failed on our
// side (a store's error, which the around wraps: "couldn't make the project:
// <driver text>"), so it is logged and answered 500 in words, never with the
// store's own text, which names constraints and tables (CR-444 F2; the
// project's own create answers the same way).
func installFailed(w http.ResponseWriter, r *http.Request, err error) {
	if answered(w, err) {
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/InstallTemplate couldn't install the template err: %+v", err)
	helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't install the template. Please try again."})
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
	// The body is optional: none, or {}, installs as it always has; a table
	// template's project_team_uuid also makes its project and channel.
	var opts business.InstallOptions
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&opts); err != nil && !errors.Is(err, io.EOF) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid install options"})
		return
	}
	userInfo := currentUser(r)
	res, ierr := installTemplate(r.Context(), id, &userInfo, opts)
	if ierr != nil {
		installFailed(w, r, ierr)
		return
	}
	details := map[string]interface{}{"template_id": id.String(), "kind": res.Kind, "entity_id": res.EntityID}
	if res.ProjectID != "" {
		details["project_id"], details["channel_id"] = res.ProjectID, res.ChannelID
	}
	auditBusiness.Record(r, "marketplace.install", auditBusiness.CategoryApp,
		"Installed template: "+res.Name, details)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}
