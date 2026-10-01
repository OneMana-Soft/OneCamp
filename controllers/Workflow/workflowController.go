package controllers

// Admin HTTP handlers for the Workflow Builder. Mounted under /admin (guarded
// by the admin auth middleware), like webhooks and the app platform —
// workspace automation is an admin concern. Every mutation is audit-logged.

import (
	"encoding/json"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// workflowRequest is the create/update body.
type workflowRequest struct {
	Name          string                            `json:"name"`
	IsActive      bool                              `json:"is_active"`
	TriggerType   string                            `json:"trigger_type"`
	TriggerConfig map[string]interface{}            `json:"trigger_config"`
	BotName       string                            `json:"bot_name"`
	ChannelID     string                            `json:"channel_id"`
	Keywords      []string                          `json:"keywords"`
	MatchType     string                            `json:"match_type"`
	Actions       []workflowBusiness.WorkflowAction `json:"actions"`
}

func (req *workflowRequest) toInput() workflowBusiness.WorkflowInput {
	return workflowBusiness.WorkflowInput{
		Name:          req.Name,
		IsActive:      req.IsActive,
		TriggerType:   req.TriggerType,
		TriggerConfig: req.TriggerConfig,
		BotName:       req.BotName,
		ChannelID:     req.ChannelID,
		Keywords:      req.Keywords,
		MatchType:     req.MatchType,
		Actions:       req.Actions,
	}
}

// actorFrom builds a workflow Actor from the request's authenticated user.
func actorFrom(r *http.Request) workflowBusiness.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return workflowBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}
}

// writeManageErr maps a business manage-error to the right HTTP status.
func writeManageErr(w http.ResponseWriter, err error) {
	switch {
	case workflowBusiness.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you can only manage workflows you created"})
	case workflowBusiness.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "workflow not found"})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// ListWorkflows handles GET /workflows — scoped to the actor (admins see all,
// members see their own).
func ListWorkflows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := workflowBusiness.ListWorkflows(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListWorkflows err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load workflows"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetWorkflow handles GET /workflows/{id}
func GetWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid workflow id"})
		return
	}
	wf, err := workflowBusiness.GetWorkflow(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	if wf == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "workflow not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": wf})
}

// CreateWorkflow handles POST /workflows
func CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req workflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	wf, err := workflowBusiness.CreateWorkflow(ctx, req.toInput(), userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	auditBusiness.Record(r, "workflow.create", auditBusiness.CategorySettings,
		"Created workflow: "+wf.Name, map[string]interface{}{"workflow_id": wf.Id.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": wf})
}

// DraftWorkflow handles POST /workflows/draft — turn a natural-language prompt
// into a draft workflow (never saved) to pre-fill the builder. Body:
// { "prompt": string }.
func DraftWorkflow(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
}

// UpdateWorkflow handles PUT /workflows/{id}
func UpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid workflow id"})
		return
	}

	var req workflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	wf, err := workflowBusiness.UpdateWorkflow(ctx, id, req.toInput(), actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}

	auditBusiness.Record(r, "workflow.update", auditBusiness.CategorySettings,
		"Updated workflow: "+wf.Name, map[string]interface{}{"workflow_id": wf.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": wf})
}

// SetWorkflowActive handles POST /workflows/{id}/active
func SetWorkflowActive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid workflow id"})
		return
	}

	var body struct {
		IsActive bool `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	if err := workflowBusiness.SetActive(ctx, id, body.IsActive, actorFrom(r)); err != nil {
		writeManageErr(w, err)
		return
	}

	auditBusiness.Record(r, "workflow.toggle", auditBusiness.CategorySettings,
		"Toggled workflow", map[string]interface{}{"workflow_id": id.String(), "is_active": body.IsActive})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// DeleteWorkflow handles DELETE /workflows/{id}
func DeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid workflow id"})
		return
	}

	if err := workflowBusiness.DeleteWorkflow(ctx, id, actorFrom(r)); err != nil {
		writeManageErr(w, err)
		return
	}

	auditBusiness.Record(r, "workflow.delete", auditBusiness.CategorySettings,
		"Deleted workflow", map[string]interface{}{"workflow_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}
