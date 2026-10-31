package controllers

// A project's rules (package 9 R1): members read them and their runs; the
// project's admins keep them (Work Access NeedManage); undoing a run needs
// the right to change the tasks it touched, checked per change by the Task
// Writer as the person undoing.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ruleRequest is a project rule's create/update body.
type ruleRequest struct {
	Name          string                            `json:"name"`
	IsActive      bool                              `json:"is_active"`
	TriggerType   string                            `json:"trigger_type"`
	TriggerConfig map[string]interface{}            `json:"trigger_config"`
	ChannelID     string                            `json:"channel_id"`
	Conditions    workflowBusiness.Conditions       `json:"conditions"`
	Actions       []workflowBusiness.WorkflowAction `json:"actions"`
	ElseActions   []workflowBusiness.WorkflowAction `json:"else_actions"`
}

func (req *ruleRequest) toInput() workflowBusiness.WorkflowInput {
	return workflowBusiness.WorkflowInput{
		Name: req.Name, IsActive: req.IsActive, TriggerType: req.TriggerType, TriggerConfig: req.TriggerConfig,
		ChannelID: req.ChannelID, Conditions: req.Conditions, Actions: req.Actions, ElseActions: req.ElseActions,
	}
}

// ruleView is a rule as the app reads it: its JSON parsed.
type ruleView struct {
	ID            uuid.UUID                         `json:"id"`
	Name          string                            `json:"name"`
	IsActive      bool                              `json:"is_active"`
	CreatedBy     uuid.UUID                         `json:"created_by"`
	TriggerType   string                            `json:"trigger_type"`
	TriggerConfig map[string]interface{}            `json:"trigger_config"`
	ChannelID     *uuid.UUID                        `json:"channel_id,omitempty"`
	Conditions    workflowBusiness.Conditions       `json:"conditions"`
	Actions       []workflowBusiness.WorkflowAction `json:"actions"`
	ElseActions   []workflowBusiness.WorkflowAction `json:"else_actions"`
	RunCount      int64                             `json:"run_count"`
	LastRunAt     *time.Time                        `json:"last_run_at,omitempty"`
	LastError     *string                           `json:"last_error,omitempty"`
	PausedReason  *string                           `json:"paused_reason,omitempty"`
	// NextRunAt is a schedule's next run, while it is on.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func viewOf(w *workflowModel.Workflow) ruleView {
	v := ruleView{ID: w.Id, Name: w.Name, IsActive: w.IsActive, CreatedBy: w.CreatedBy, TriggerType: w.TriggerType,
		ChannelID: w.ChannelId, RunCount: w.RunCount, LastRunAt: w.LastRunAt, LastError: w.LastError,
		PausedReason: w.PausedReason, UpdatedAt: w.UpdatedAt, NextRunAt: workflowBusiness.NextRun(w),
		TriggerConfig: map[string]interface{}{}, Actions: []workflowBusiness.WorkflowAction{}, ElseActions: []workflowBusiness.WorkflowAction{}}
	_ = json.Unmarshal([]byte(w.TriggerConfig), &v.TriggerConfig)
	_ = json.Unmarshal([]byte(w.Conditions), &v.Conditions)
	_ = json.Unmarshal([]byte(w.Actions), &v.Actions)
	_ = json.Unmarshal([]byte(w.ElseActions), &v.ElseActions)
	return v
}

func caller(r *http.Request) *userModels.UserInfo {
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return &u
}

func ruleID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a rule."})
		return uuid.Nil, false
	}
	return id, true
}

// writeRuleErr says why a rule change didn't happen.
func writeRuleErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workflowBusiness.ErrThroughToken):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Rules are set in the app, not through a token."})
	case workflowBusiness.IsNotThisProject(err), errors.Is(err, workflowModel.ErrRunNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That isn't in this project."})
	case errors.Is(err, workflowBusiness.ErrNotUndoable):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "There's nothing left to undo in that run."})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// ListProjectRules handles GET /project/{project_uuid}/rules.
func ListProjectRules(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	rules, err := workflowBusiness.ListProjectRules(r.Context(), projectID)
	if err != nil {
		helpers.WriteRetryLater(w, "Couldn't read this project's rules.")
		return
	}
	views := make([]ruleView, 0, len(rules))
	for _, rule := range rules {
		views = append(views, viewOf(rule))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"rules": views, "can_manage": s.Can(workAccess.NeedManage) && !helpers.ActsThroughToken(r.Context()),
		"templates": workflowBusiness.Templates(), "max": workflowModel.MaxRulesPerProject, "agents": workflowBusiness.AgentsAvailable(),
	}})
}

// CreateProjectRule handles POST /project/{project_uuid}/rules.
func CreateProjectRule(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its rules.")
	if !ok {
		return
	}
	var req ruleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That rule didn't read."})
		return
	}
	rule, err := workflowBusiness.CreateProjectRule(r.Context(), caller(r), projectID, req.toInput())
	if err != nil {
		writeRuleErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.rule.create", auditBusiness.CategorySettings, "Made rule: "+rule.Name,
		map[string]interface{}{"rule_id": rule.Id.String(), "project_id": projectID.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": viewOf(rule)})
}

// UpdateProjectRule handles POST /project/{project_uuid}/rules/{rule_id}.
func UpdateProjectRule(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its rules.")
	if !ok {
		return
	}
	id, ok := ruleID(w, r, "rule_id")
	if !ok {
		return
	}
	var req ruleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That rule didn't read."})
		return
	}
	rule, err := workflowBusiness.UpdateProjectRule(r.Context(), caller(r), projectID, id, req.toInput())
	if err != nil {
		writeRuleErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.rule.update", auditBusiness.CategorySettings, "Changed rule: "+rule.Name,
		map[string]interface{}{"rule_id": id.String(), "project_id": projectID.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": viewOf(rule)})
}

// SetProjectRuleActive handles POST /project/{project_uuid}/rules/{rule_id}/active.
func SetProjectRuleActive(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its rules.")
	if !ok {
		return
	}
	id, ok := ruleID(w, r, "rule_id")
	if !ok {
		return
	}
	var body struct {
		IsActive bool `json:"is_active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That didn't read."})
		return
	}
	if err := workflowBusiness.SetProjectRuleActive(r.Context(), caller(r), projectID, id, body.IsActive); err != nil {
		writeRuleErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.rule.active", auditBusiness.CategorySettings, "Turned a rule "+map[bool]string{true: "on", false: "off"}[body.IsActive],
		map[string]interface{}{"rule_id": id.String(), "project_id": projectID.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"is_active": body.IsActive}})
}

// DeleteProjectRule handles POST /project/{project_uuid}/rules/{rule_id}/delete.
func DeleteProjectRule(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its rules.")
	if !ok {
		return
	}
	id, ok := ruleID(w, r, "rule_id")
	if !ok {
		return
	}
	if err := workflowBusiness.DeleteProjectRule(r.Context(), projectID, id); err != nil {
		writeRuleErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.rule.delete", auditBusiness.CategorySettings, "Deleted a rule",
		map[string]interface{}{"rule_id": id.String(), "project_id": projectID.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted."})
}

// ListProjectRuleRuns handles GET /project/{project_uuid}/rules/runs?rule=&before=&limit=.
func ListProjectRuleRuns(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	var rule *uuid.UUID
	if raw := r.URL.Query().Get("rule"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a rule."})
			return
		}
		rule = &id
	}
	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "before is a time."})
			return
		}
		before = &t
	}
	limit := helpers.LimitParam(r.URL.Query(), 50, 100)
	runs, err := workflowBusiness.ListProjectRuns(r.Context(), projectID, rule, before, limit)
	if err != nil {
		writeRuleErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": runs})
}

// UndoProjectRuleRun handles POST /project/{project_uuid}/rules/runs/{run_id}/undo.
func UndoProjectRuleRun(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedEditTask, "You can't change this project's tasks.")
	if !ok {
		return
	}
	id, ok := ruleID(w, r, "run_id")
	if !ok {
		return
	}
	run, err := workflowBusiness.UndoRun(r.Context(), caller(r), projectID, id)
	if err != nil {
		writeRuleErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": run})
}
