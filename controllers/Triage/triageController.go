// Package controllers (Triage) is the Triage Queue's routes (package 9 R5a,
// business/Triage): a project's queue beside /project, which its people
// read, and a decision beside /task, for those who can change the task. A
// project turns triage on through its policies (PUT
// /project/{project_uuid}/policies/projects.triage).
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// writeErr says why a decision wasn't made. A move the project's status rules
// hold back is said as a status change anywhere says it (409, with the request
// it became when it needs an approval).
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if projectaccess.RefuseStatus(w, err) {
		return
	}
	switch {
	case errors.Is(err, taskBusiness.ErrTaskNotVisible):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That task isn't there."})
	case errors.Is(err, workAccess.ErrNotAllowed):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, triageBusiness.ErrNotWaiting):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That has been decided already."})
	case errors.Is(err, triageBusiness.ErrNeedsPriority):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Give it a priority before accepting it."})
	case errors.Is(err, triageBusiness.ErrTriageOff):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This project has triage off."})
	case errors.Is(err, taskStatusBusiness.ErrUnknownStatus):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That is not a status in this project."})
	case errors.Is(err, triageBusiness.ErrRotaThroughToken):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "The triage rota is set in the app, not through a token."})
	case errors.Is(err, triageBusiness.ErrBadRota):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": strings.TrimPrefix(err.Error(), triageBusiness.ErrBadRota.Error()+": ")})
	case errors.Is(err, triageBusiness.ErrBadChoice):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": strings.TrimPrefix(err.Error(), triageBusiness.ErrBadChoice.Error()+": ")})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Triage err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't do that in triage.")
	}
}

// Queue handles GET /project/{project_uuid}/triage: whether the project has
// triage on, what waits in it that the reader may read, and whether they may
// decide.
func Queue(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	requests, err := triageBusiness.Triage.Waiting(r.Context(), &me, projectID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// Nothing waiting goes as "requests": [], as every empty list does
	// (helpers.WriteJSON, V-52).
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"on": triageBusiness.On(r.Context(), projectID), "requests": requests, "can_decide": s.Can(workAccess.NeedEditTask),
	}})
}

// Decide handles POST /task/triage/{task_uuid}/decide {decision, reason,
// status, duplicate_of, until}: accept (into status, when given), duplicate
// (of duplicate_of), decline, or snooze (until an RFC 3339 time).
func Decide(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	var in struct {
		Decision    string `json:"decision"`
		Reason      string `json:"reason"`
		Status      string `json:"status"`
		DuplicateOf string `json:"duplicate_of"`
		Until       string `json:"until"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That decision didn't read."})
		return
	}
	c := triageBusiness.Choice{Decision: triageBusiness.Decision(in.Decision), Reason: in.Reason, Status: strings.TrimSpace(in.Status)}
	if in.DuplicateOf != "" {
		if c.DuplicateOf, err = uuid.Parse(in.DuplicateOf); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task to mark it a duplicate of."})
			return
		}
	}
	if in.Until != "" {
		if c.Until, err = time.Parse(time.RFC3339, in.Until); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a time to snooze it until."})
			return
		}
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	if err := triageBusiness.Triage.Decide(r.Context(), &me, taskID, c); err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Decided."})
}

// Rota handles GET /project/{project_uuid}/triage/rota: who is on the
// project's triage duty, in turns, and who is on now (linear-21 H2). For
// whoever the queue admits; can_manage says whether the reader may set it.
// The route's one check hands the standing down: nothing is read twice.
func Rota(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	rota, err := triageBusiness.ReadRota(r.Context(), s, projectID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"rota": rota, "can_manage": rota.CanManage}})
}

// SetRota handles PUT /project/{project_uuid}/triage/rota {people, weeks,
// start}: the project's admins set its triage rota, in the app. Recorded in
// the audit log.
func SetRota(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, "Only the project's admins choose who is on triage duty.")
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in triageBusiness.Rota
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	rota, err := triageBusiness.SetRota(r.Context(), &me, s, projectID, in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"rota": rota}, "msg": "Saved."})
}
