package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	milestoneBusiness "github.com/akashc777/OneCamp/business/Milestone"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Milestones: see business/Milestone. Whoever may see a project sees the
// milestones it shows; its admins make them, order its list and take a
// shared one off it; the admins of a milestone's home project rename, date,
// share and delete it; whoever may edit a task puts it in one of the
// milestones its project shows.

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var me *milestoneBusiness.MilestoneError
	if errors.As(err, &me) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": me.Error()})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/Milestone/%s err: %+v", where, err)
	helpers.WriteRetryLater(w, "Something went wrong.")
}

// readerOf is the caller as milestones are read for them.
func readerOf(u userModels.UserInfo) milestoneBusiness.Reader {
	return milestoneBusiness.Reader{UUID: u.UserDgraphInfo.Uuid, UID: u.UserDgraphInfo.Uid}
}

// decode reads a request's JSON body into v, or answers that it couldn't.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(v); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return false
	}
	return true
}

// project checks the caller may do need with the project in the URL: see its
// milestones, or (NeedManage) change them.
func project(w http.ResponseWriter, r *http.Request, need workAccess.Need) (uuid.UUID, workAccess.Standing, bool) {
	id, _, s, ok := projectaccess.RequireStanding(w, r, need, adminsOnly)
	return id, s, ok
}

// adminsOnly is the refusal of a change to a project's milestones.
const adminsOnly = "Only the project's admins can change its milestones."

// milestoneIn reads the milestone in the URL, which the project must show.
func milestoneIn(w http.ResponseWriter, r *http.Request, project uuid.UUID) (*milestoneModel.Milestone, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "milestone_id"))
	if err == nil {
		m, err := milestoneModel.Get(r.Context(), id)
		if err != nil {
			write(w, r, "milestoneIn", err)
			return nil, false
		}
		if m != nil && milestoneBusiness.ShowsIn(m, project.String()) {
			return m, true
		}
	}
	helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Milestone not found"})
	return nil, false
}

// home refuses a change to milestone m from a project it was only shared
// with: the project that made it keeps it.
func home(w http.ResponseWriter, m *milestoneModel.Milestone, project uuid.UUID) bool {
	if m.ProjectUUID == project {
		return true
	}
	helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This milestone is another project's: its admins change it there."})
	return false
}

// ListMilestones is the milestones a project shows, in its order, with each
// one's progress and span over the tasks the caller may open.
// GET /project/{p}/milestones
func ListMilestones(w http.ResponseWriter, r *http.Request) {
	id, s, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	views, err := milestoneBusiness.List(r.Context(), id, readerOf(u))
	if err != nil {
		write(w, r, "ListMilestones", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"milestones": views,
		"can_manage": !s.Archived && s.Can(workAccess.NeedManage),
	}})
}

// MyMilestones is the milestones of every project the caller is in, by
// project: the projects overview and its timeline. GET /project/milestones
func MyMilestones(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	all, err := milestoneBusiness.Mine(r.Context(), readerOf(u))
	if err != nil {
		write(w, r, "MyMilestones", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"projects": all}})
}

// CreateMilestone makes a milestone, last in the project's order.
// POST /project/{p}/milestones {name, description, target_date}
func CreateMilestone(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	var in milestoneBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	m, err := milestoneBusiness.Create(r.Context(), id, in, u.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "CreateMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": m})
}

// EditMilestone sets a milestone's name, description and target day.
// POST /project/{p}/milestones/{m}/edit {name, description, target_date}
func EditMilestone(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	m, ok := milestoneIn(w, r, id)
	if !ok || !home(w, m, id) {
		return
	}
	var in milestoneBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	if err := milestoneBusiness.Edit(r.Context(), m, in); err != nil {
		write(w, r, "EditMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved"})
}

// DeleteMilestone deletes a milestone from every project it shows in; its
// tasks leave it. POST /project/{p}/milestones/{m}/delete
func DeleteMilestone(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	m, ok := milestoneIn(w, r, id)
	if !ok || !home(w, m, id) {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	if err := milestoneModel.Delete(r.Context(), m.Id, u.UserPostgresInfo.Id); err != nil {
		write(w, r, "DeleteMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// ReorderMilestones puts the project's milestones in the order given.
// POST /project/{p}/milestones/reorder {ids}
func ReorderMilestones(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := milestoneBusiness.Reorder(r.Context(), id, in.IDs); err != nil {
		write(w, r, "ReorderMilestones", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved"})
}

// ShareMilestone shows a milestone in another project too, whose tasks may
// then join it: for the admins of its home who also run that project.
// POST /project/{p}/milestones/{m}/share {project_uuid}
func ShareMilestone(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	m, ok := milestoneIn(w, r, id)
	if !ok || !home(w, m, id) {
		return
	}
	var in struct {
		ProjectUUID string `json:"project_uuid"`
	}
	if !decode(w, r, &in) {
		return
	}
	to, _, ok := projectaccess.RequireID(w, r, in.ProjectUUID, workAccess.NeedManage, "Only that project's admins can add a milestone to it.")
	if !ok {
		return
	}
	if err := milestoneBusiness.Share(r.Context(), m, to); err != nil {
		write(w, r, "ShareMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Shared"})
}

// UnshareMilestone takes a milestone off a project it was shared with, and
// that project's tasks out of it: the project's admins from its own list, or
// the home's admins from any project they shared it with.
// POST /project/{p}/milestones/{m}/unshare {project_uuid} (none: this project)
func UnshareMilestone(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	m, ok := milestoneIn(w, r, id)
	if !ok {
		return
	}
	var in struct {
		ProjectUUID string `json:"project_uuid"`
	}
	if !decode(w, r, &in) {
		return
	}
	from := id
	if in.ProjectUUID != "" {
		parsed, err := uuid.Parse(in.ProjectUUID)
		if err != nil || !milestoneBusiness.ShowsIn(m, parsed.String()) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That project doesn't show this milestone."})
			return
		}
		from = parsed
	}
	if from != id && !home(w, m, id) {
		return
	}
	if err := milestoneBusiness.Unshare(r.Context(), m, from); err != nil {
		write(w, r, "UnshareMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Taken off"})
}

// projectOf is a task's project's id, "" for none.
func projectOf(t *dgraphStruct.DgraphTask) string {
	if t.Project == nil {
		return ""
	}
	return t.Project.Uuid
}

// GetTaskMilestone is the milestone a task is in (null when none).
// GET /task/milestone/{task_uuid}
func GetTaskMilestone(w http.ResponseWriter, r *http.Request) {
	taskID, task, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_uuid"), workAccess.NeedView)
	if !ok {
		return
	}
	m, err := milestoneBusiness.Of(r.Context(), taskID, projectOf(task))
	if err != nil {
		write(w, r, "GetTaskMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": m})
}

// SetTaskMilestone puts a task in one of the milestones its project shows, or
// takes it out of its own (milestone_id ""). Putting a task in a milestone
// is changing the task. POST /task/milestone {task_uuid, milestone_id}
func SetTaskMilestone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskUUID    string `json:"task_uuid"`
		MilestoneID string `json:"milestone_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	taskID, task, ok := projectaccess.RequireTask(w, r, in.TaskUUID, workAccess.NeedEditTask)
	if !ok {
		return
	}
	project := projectOf(task)
	_, refused, err := milestoneBusiness.Put(r.Context(), map[uuid.UUID]string{taskID: project}, in.MilestoneID)
	if err == nil {
		err = refused[taskID]
	}
	if err != nil {
		write(w, r, "SetTaskMilestone", err)
		return
	}
	m, err := milestoneBusiness.Of(r.Context(), taskID, project)
	if err != nil {
		write(w, r, "SetTaskMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": m})
}
