// Package controllers (SLA) is a project's SLA settings over HTTP
// (business/SLA): its members read them; its admins change its rules and its
// work week, each change audited or not made. Turning its timers on or off is
// its projects.sla policy (PUT /project/{project_uuid}/policies/projects.sla).
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	slaBusiness "github.com/akashc777/OneCamp/business/SLA"
	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const bodyLimit = 64 << 10

// member reads the project in the URL for one of its people (NeedView):
// someone outside it finds nothing. What they may change, business/SLA says.
func member(w http.ResponseWriter, r *http.Request) (uuid.UUID, workAccess.Standing, bool) {
	id, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	return id, s, ok
}

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var refused *slaBusiness.Refused
	var problem *slaBusiness.Problem
	switch {
	case errors.As(err, &refused):
		helpers.WriteForbidden(w, refused.Error())
	case errors.As(err, &problem):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": problem.Error()})
	case errors.Is(err, auditBusiness.ErrNotRecorded):
		helpers.WriteRetryLater(w, "The change couldn't be recorded, so it wasn't made.")
	default:
		helpers.WriteReadFailure(w, r, err, "SLA rule", "controllers/SLA/"+where)
	}
}

func read(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(v); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return false
	}
	return true
}

// Show GET /project/{project_uuid}/sla: whether its timers run, its
// rules (the defaults until its admins change them) and its work week.
func Show(w http.ResponseWriter, r *http.Request) {
	id, s, ok := member(w, r)
	if !ok {
		return
	}
	v, err := slaBusiness.Settings(r.Context(), s, id)
	if err != nil {
		write(w, r, "Show", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// SetHours PUT /project/{project_uuid}/sla/hours {"hours": {...} | null}.
func SetHours(w http.ResponseWriter, r *http.Request) {
	id, s, ok := member(w, r)
	var in struct {
		Hours *weekHours.Hours `json:"hours"`
	}
	if !ok || !read(w, r, &in) {
		return
	}
	h, err := slaBusiness.SetHours(r.Context(), s, id, in.Hours)
	if err != nil {
		write(w, r, "SetHours", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"hours": h, "hours_set": in.Hours != nil}})
}

// AddRule POST /project/{project_uuid}/sla/rules {name, priorities, labels,
// type_ids, sources, target_minutes, clock, rank}.
func AddRule(w http.ResponseWriter, r *http.Request) {
	id, s, ok := member(w, r)
	var in slaBusiness.RuleInput
	if !ok || !read(w, r, &in) {
		return
	}
	rule, err := slaBusiness.AddRule(r.Context(), s, id, in)
	if err != nil {
		write(w, r, "AddRule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": rule})
}

// ruleID is the rule the URL names; a bad one is not found.
func ruleID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "rule_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That SLA rule isn't there."})
	}
	return id, err == nil
}

// ChangeRule PATCH /project/{project_uuid}/sla/rules/{rule_id}, as AddRule.
func ChangeRule(w http.ResponseWriter, r *http.Request) {
	id, s, ok := member(w, r)
	if !ok {
		return
	}
	rid, ok := ruleID(w, r)
	var in slaBusiness.RuleInput
	if !ok || !read(w, r, &in) {
		return
	}
	rule, err := slaBusiness.ChangeRule(r.Context(), s, id, rid, in)
	if err != nil {
		write(w, r, "ChangeRule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rule})
}

// RemoveRule DELETE /project/{project_uuid}/sla/rules/{rule_id}: kept as
// removed; its timers run to their end.
func RemoveRule(w http.ResponseWriter, r *http.Request) {
	id, s, ok := member(w, r)
	if !ok {
		return
	}
	rid, ok := ruleID(w, r)
	if !ok {
		return
	}
	if err := slaBusiness.RemoveRule(r.Context(), s, id, rid); err != nil {
		write(w, r, "RemoveRule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed"})
}

// RestoreRule is POST /restore/sla_rule/{id} (controllers/Restore): a removed
// rule back, by its project's admins, as its removal (someone outside the
// project finds nothing; a member is refused in business/SLA's words).
func RestoreRule(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	project, _, s, ok := projectaccess.RequireIDStanding(w, r, owner.Project.String(), workAccess.NeedView, "")
	if !ok {
		return
	}
	if err := slaBusiness.RestoreRule(r.Context(), s, project, id); err != nil {
		write(w, r, "RestoreRule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "SLA rule restored."})
}
