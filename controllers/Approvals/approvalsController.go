// Package controllers (Approvals) is a project's status rules and a task's
// approval requests over HTTP (package 9 R2, business/Approvals).
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskBusiness.ErrTaskNotVisible):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That task isn't there."})
	case errors.Is(err, approvalsBusiness.ErrNotApprover), errors.Is(err, approvalsBusiness.ErrNotYours), errors.Is(err, approvalsBusiness.ErrThroughToken):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, approvalsBusiness.ErrNotOpen):
		// Answered, withdrawn, or closed when its task moved on: the card it
		// was pressed on reads the request again and says which.
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That request is closed now."})
	case errors.Is(err, approvalsBusiness.ErrWaitingAlready):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "It's waiting for approval already."})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// ListStatusRules handles GET /project/{project_uuid}/statusRules.
func ListStatusRules(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	rules, err := approvalsBusiness.ListRules(r.Context(), projectID)
	if err != nil {
		helpers.WriteRetryLater(w, "Couldn't read this project's status rules.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"rules": rules, "can_manage": s.Can(workAccess.NeedManage) && !helpers.ActsThroughToken(r.Context()), "max": model.MaxRulesPerProject,
	}})
}

// SaveStatusRule handles POST /project/{project_uuid}/statusRules.
func SaveStatusRule(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its status rules.")
	if !ok {
		return
	}
	var in approvalsBusiness.RuleInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That rule didn't read."})
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	rule, err := approvalsBusiness.SaveRule(r.Context(), me.UserPostgresInfo.Id, projectID, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.status_rule.save", auditBusiness.CategorySettings, "Set a status rule",
		map[string]interface{}{"project_id": projectID.String(), "status": in.Status})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rule})
}

// DeleteStatusRule handles POST /project/{project_uuid}/statusRules/delete.
func DeleteStatusRule(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its status rules.")
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That didn't read."})
		return
	}
	if err := approvalsBusiness.DeleteRule(r.Context(), projectID, in.Status); err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "project.status_rule.delete", auditBusiness.CategorySettings, "Removed a status rule",
		map[string]interface{}{"project_id": projectID.String(), "status": in.Status})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed."})
}

func taskID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return uuid.Nil, false
	}
	return id, true
}

// TaskApproval handles GET /task/approval/{task_uuid}: its newest request.
func TaskApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	v, err := approvalsBusiness.ForTask(r.Context(), &me, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// DecideApproval handles POST /task/approval/{task_uuid}/decide.
func DecideApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var in struct {
		ApprovalID string `json:"approval_id"`
		Verdict    string `json:"verdict"`
		Comment    string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That answer didn't read."})
		return
	}
	approval, err := uuid.Parse(in.ApprovalID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a request."})
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	v, err := approvalsBusiness.Decide(r.Context(), &me, id, approval, in.Verdict, in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// WithdrawApproval handles POST /task/approval/{task_uuid}/withdraw.
func WithdrawApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	if err := approvalsBusiness.Withdraw(r.Context(), &me, id); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Withdrawn."})
}

// WaitingApprovals handles GET /approvals/waiting: the requests waiting for
// the reader's answer, each read as them (package 9 R2b).
func WaitingApprovals(w http.ResponseWriter, r *http.Request) {
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	items, err := approvalsBusiness.WaitingFor(r.Context(), &me)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"items": items}})
}

// AskApproval handles POST /task/approval/{task_uuid}/ask: asks for the task
// itself to be approved (package 9 R2b), by approvers among the project's
// members and, when asked, its clients.
func AskApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var in approvalsBusiness.AskInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That ask didn't read."})
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	v, err := approvalsBusiness.AskForTask(r.Context(), &me, id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}
