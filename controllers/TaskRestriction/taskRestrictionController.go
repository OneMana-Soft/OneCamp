// Package controllers (TaskRestriction) is a task's restriction, as an API
// (WA2, design/projects-vs-asana-jira/WA2_PLAN.md): who besides its project's
// admins, its assignee and its creator may see a task its project keeps to a
// few people. The rule is business/WorkAccess's; models/taskread keeps a
// restricted task from everyone else, everywhere.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// bodyLimit is the most a restriction's request may be: a list of people.
const bodyLimit = 64 << 10

// maxPeople is the most people a task may be let into by name.
const maxPeople = 200

// Seams for tests.
var (
	restrictionOf = restrictionModel.Of
	now           = time.Now
)

// answer is a task's restriction as the app shows it.
type answer struct {
	TaskUUID   string   `json:"task_uuid"`
	Restricted bool     `json:"restricted"`
	People     []string `json:"people"`
	By         string   `json:"restricted_by,omitempty"`
	At         string   `json:"restricted_at,omitempty"`
	// With is the task above it that it is restricted with, whose people are
	// its people (People); "" when it is restricted on its own.
	With string `json:"restricted_with,omitempty"`
	// CanChange is whether the reader may restrict it or open it again.
	CanChange bool `json:"can_change"`
}

func me(r *http.Request) userModels.UserInfo {
	if u, ok := httpx.UserFrom(r); ok && u != nil {
		return *u
	}
	return userModels.UserInfo{}
}

// Restriction GET /task/{task_ref}/restriction: whether the task is
// restricted, who is let in by name, and whether the reader may change that.
// Anyone who may see the task may read it.
func Restriction(w http.ResponseWriter, r *http.Request) {
	id, task, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_ref"), workAccess.NeedView)
	if !ok {
		return
	}
	caller := me(r)
	s, f, err := workAccess.OfTask(r.Context(), caller.UserDgraphInfo.Uuid, caller.UserDgraphInfo.Uid, task)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskRestriction.Restriction err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't read who can see this task.")
		return
	}
	got, err := restrictionOf(r.Context(), id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskRestriction.Restriction err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't read who can see this task.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answerOf(id, got, s.MayRestrict(f))})
}

func answerOf(id uuid.UUID, r restrictionModel.Restriction, canChange bool) answer {
	a := answer{TaskUUID: id.String(), Restricted: r.Restricted, People: []string{}, CanChange: canChange}
	if !r.Restricted {
		return a
	}
	for _, p := range r.Grants {
		a.People = append(a.People, p.String())
	}
	if r.RestrictedBy != uuid.Nil {
		a.By = r.RestrictedBy.String()
	}
	if !r.RestrictedAt.IsZero() {
		a.At = r.RestrictedAt.UTC().Format(time.RFC3339)
	}
	if r.Via != uuid.Nil {
		a.With = r.Via.String()
	}
	return a
}

// restrictionInput is a request to restrict a task, or open it again.
type restrictionInput struct {
	Restricted bool     `json:"restricted"`
	People     []string `json:"people"`
}

// SetRestriction PUT /task/{task_ref}/restriction: keeps the task to its
// project's admins, its assignee, its creator and people (user ids, which
// must be the project's people; those named before are replaced), with the
// tasks below it that aren't restricted already, or with restricted false
// opens it to its project again, with those. Its project's admins may, and
// its creator while they may change it.
func SetRestriction(w http.ResponseWriter, r *http.Request) {
	var in restrictionInput
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	people, bad := parsePeople(in.People)
	switch {
	case bad != "":
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "\"" + bad + "\" isn't a person."})
		return
	case len(people) > maxPeople:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task can be kept to at most 200 people by name. Restrict the project instead."})
		return
	case !in.Restricted && len(people) > 0:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task its whole project can see lets no one in by name."})
		return
	}
	id, task, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_ref"), workAccess.NeedView)
	if !ok {
		return
	}
	caller := me(r)
	s, f, err := workAccess.OfTask(r.Context(), caller.UserDgraphInfo.Uuid, caller.UserDgraphInfo.Uid, task)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskRestriction.SetRestriction err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't check who can see this task.")
		return
	}
	if !s.MayRestrict(f) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only this project's admins, or whoever made the task, can choose who sees it."})
		return
	}
	_, err = taskBusiness.SetRestriction(r.Context(), task, in.Restricted, people, taskBusiness.Changer{
		By: caller.UserPostgresInfo.Id, UserUUID: caller.UserDgraphInfo.Uuid,
		Record: func(action, summary string, meta map[string]any) {
			auditBusiness.Record(r, action, auditBusiness.CategorySecurity, summary, meta)
		},
	}, now())
	var notPeople *workAccess.NotProjectPeopleError
	switch {
	case errors.As(err, &notPeople):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Only people in the task's project can be let into it.", "user_uuid": notPeople.UserUUID})
		return
	case errors.Is(err, workAccess.ErrNoProject):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task in no project is its maker's already."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskRestriction.SetRestriction err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change who can see this task.")
		return
	}
	got, err := restrictionOf(r.Context(), id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answerOf(id, got, true)})
}

// parsePeople reads the people a task is let into, dropping repeats; bad is
// the first entry that isn't a user id.
func parsePeople(raw []string) (people []uuid.UUID, bad string) {
	seen := map[uuid.UUID]bool{}
	for _, p := range raw {
		id, err := uuid.Parse(strings.TrimSpace(p))
		if err != nil || id == uuid.Nil {
			return nil, p
		}
		if !seen[id] {
			seen[id] = true
			people = append(people, id)
		}
	}
	return people, ""
}
