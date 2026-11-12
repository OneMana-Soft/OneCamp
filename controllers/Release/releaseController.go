// Package controllers (Release) is a project's releases over HTTP
// (business/Release). Its members read them; its admins make, change, ship,
// fill and delete them, as they do its cycles. Once the project is archived
// its releases read as before and change no more, as its milestones.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	releaseBusiness "github.com/akashc777/OneCamp/business/Release"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// adminsOnly and archived are the refusals of a change to a project's
// releases.
const (
	adminsOnly = "Only the project's admins can change its releases."
	archived   = "This project is archived, so its releases can't change. Restore the project first."
)

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var re *releaseBusiness.ReleaseError
	if errors.As(err, &re) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": re.Error()})
		return
	}
	var no *releaseBusiness.Refused
	if errors.As(err, &no) {
		helpers.WriteForbidden(w, no.Error())
		return
	}
	helpers.WriteReadFailure(w, r, err, "release", "controllers/Release/"+where)
}

// editable refuses, in words, a change by standing s (an admin's, checked
// already) to an archived project's releases.
func editable(w http.ResponseWriter, s workAccess.Standing) bool {
	if releaseBusiness.Editable(s) {
		return true
	}
	helpers.WriteForbidden(w, archived)
	return false
}

// admin checks the caller may change the releases of the project in the URL:
// one of its admins, while it isn't archived.
func admin(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, adminsOnly)
	return id, ok && editable(w, standing)
}

// manage checks the caller may change the project's releases (admin), and
// reads the release in it named by the URL (release_id).
func manage(w http.ResponseWriter, r *http.Request) (releaseModel.Release, accessBusiness.Subject, bool) {
	id, ok := admin(w, r)
	if !ok {
		return releaseModel.Release{}, accessBusiness.Subject{}, false
	}
	_, who := accessBusiness.RequestSubject(r)
	rid, err := uuid.Parse(chi.URLParam(r, "release_id"))
	if err != nil {
		write(w, r, "manage", helpers.ErrNotFound)
		return releaseModel.Release{}, who, false
	}
	rel, found, err := releaseModel.Get(r.Context(), rid)
	if err == nil && (!found || rel.ProjectID != id) {
		err = helpers.ErrNotFound
	}
	if err != nil {
		write(w, r, "manage", err)
		return releaseModel.Release{}, who, false
	}
	return rel, who, true
}

// ListReleases is a page of a project's releases as the caller sees them.
// GET /project/{project_uuid}/releases?limit=&cursor= (next_cursor, "" on the
// last page)
func ListReleases(w http.ResponseWriter, r *http.Request) {
	project, err := uuid.Parse(chi.URLParam(r, "project_uuid"))
	if err != nil {
		write(w, r, "ListReleases", helpers.ErrNotFound)
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	q := r.URL.Query()
	list, err := releaseBusiness.Releases.List(r.Context(), who, project, q.Get("cursor"),
		helpers.LimitParam(q, releaseBusiness.PageSize, releaseBusiness.PageMost))
	if err != nil {
		write(w, r, "ListReleases", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": list})
}

// GetRelease is one release with the tasks the caller may see.
// GET /project/{project_uuid}/releases/{release_id}
func GetRelease(w http.ResponseWriter, r *http.Request) {
	project, perr := uuid.Parse(chi.URLParam(r, "project_uuid"))
	id, err := uuid.Parse(chi.URLParam(r, "release_id"))
	if perr != nil || err != nil {
		write(w, r, "GetRelease", helpers.ErrNotFound)
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	rel, err := releaseBusiness.Releases.Get(r.Context(), who, id)
	if err == nil && rel.ProjectID != project {
		err = helpers.ErrNotFound
	}
	if err != nil {
		write(w, r, "GetRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rel})
}

// ReleaseBurndown is a release's burndown in the caller's days (tz), over the
// tasks they may see. GET /project/{project_uuid}/releases/{release_id}/burndown?tz=
func ReleaseBurndown(w http.ResponseWriter, r *http.Request) {
	project, perr := uuid.Parse(chi.URLParam(r, "project_uuid"))
	id, err := uuid.Parse(chi.URLParam(r, "release_id"))
	if perr != nil || err != nil {
		write(w, r, "ReleaseBurndown", helpers.ErrNotFound)
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	view, err := releaseBusiness.Releases.Burndown(r.Context(), who, id, time.Now(), helpers.Location(r.URL.Query().Get("tz")))
	if err == nil && view.Release.ProjectID != project {
		err = helpers.ErrNotFound
	}
	if err != nil {
		write(w, r, "ReleaseBurndown", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// CreateRelease makes a planned release.
// POST /project/{project_uuid}/releases {name, target_date, notes}
func CreateRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := admin(w, r)
	if !ok {
		return
	}
	var in releaseBusiness.Input
	if !httpx.ReadJSON(w, r, 64<<10, &in) {
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	by, _ := uuid.Parse(who.UserUUID)
	rel, err := releaseBusiness.Create(r.Context(), id, in, by)
	if err != nil {
		write(w, r, "CreateRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rel})
}

// EditRelease changes a release's name, target day or notes.
// POST /project/{project_uuid}/releases/{release_id}/edit {name?, target_date?, notes?}
func EditRelease(w http.ResponseWriter, r *http.Request) {
	rel, _, ok := manage(w, r)
	if !ok {
		return
	}
	var e releaseBusiness.Edit
	if !httpx.ReadJSON(w, r, 64<<10, &e) {
		return
	}
	if err := releaseBusiness.Update(r.Context(), rel, e); err != nil {
		write(w, r, "EditRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Release saved."})
}

// ShipRelease marks a release out now and, asked to, moves its unfinished
// tasks on: move_to a planned release of the project, or "none" to take them
// out; without it every task stays. Answers how many moved.
// POST /project/{project_uuid}/releases/{release_id}/ship {move_to?}
func ShipRelease(w http.ResponseWriter, r *http.Request) {
	rel, who, ok := manage(w, r)
	if !ok {
		return
	}
	var in struct {
		MoveTo *string `json:"move_to"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		helpers.WriteBadRequest(w, "Couldn't read that request.")
		return
	}
	shipped, err := releaseBusiness.Ship(r.Context(), who, rel, in.MoveTo)
	if err != nil {
		write(w, r, "ShipRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Release marked released.", "data": shipped})
}

// DeleteRelease deletes a release softly; POST /restore/release/{id} brings
// it back. POST /project/{project_uuid}/releases/{release_id}/delete
func DeleteRelease(w http.ResponseWriter, r *http.Request) {
	rel, who, ok := manage(w, r)
	if !ok {
		return
	}
	by, _ := uuid.Parse(who.UserUUID)
	if err := releaseBusiness.Delete(r.Context(), rel.ID, by); err != nil {
		write(w, r, "DeleteRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Release deleted."})
}

// AddReleaseTasks puts tasks in a release: each a live task of the project
// the caller may see. POST /project/{project_uuid}/releases/{release_id}/tasks {task_uuids}
func AddReleaseTasks(w http.ResponseWriter, r *http.Request) {
	rel, who, ok := manage(w, r)
	if !ok {
		return
	}
	var in struct {
		Tasks []string `json:"task_uuids"`
	}
	if !httpx.ReadJSON(w, r, 16<<10, &in) {
		return
	}
	added, err := releaseBusiness.AddTasks(r.Context(), who, rel, in.Tasks)
	if err != nil {
		write(w, r, "AddReleaseTasks", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": added})
}

// RemoveReleaseTask takes a task out of a release: one the caller may see
// there; any other, as one not in it, is "Task not found".
// POST /project/{project_uuid}/releases/{release_id}/tasks/{task_uuid}/remove
func RemoveReleaseTask(w http.ResponseWriter, r *http.Request) {
	rel, who, ok := manage(w, r)
	if !ok {
		return
	}
	task, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteReadFailure(w, r, helpers.ErrNotFound, "task", "controllers/Release/RemoveReleaseTask")
		return
	}
	if err := releaseBusiness.RemoveTask(r.Context(), who, rel, task); err != nil {
		if errors.Is(err, helpers.ErrNotFound) {
			helpers.WriteReadFailure(w, r, err, "task", "controllers/Release/RemoveReleaseTask")
			return
		}
		write(w, r, "RemoveReleaseTask", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Task taken out of the release."})
}

// RestoreRelease brings a deleted release back with its tasks (POST
// /restore/release/{id}, controllers/Restore): its project's admins, as its
// delete, and not while the project is archived.
func RestoreRelease(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	_, _, standing, ok := projectaccess.RequireIDStanding(w, r, owner.Project.String(), workAccess.NeedManage, adminsOnly)
	if !ok || !editable(w, standing) {
		return
	}
	if err := releaseBusiness.Restore(r.Context(), id); err != nil {
		write(w, r, "RestoreRelease", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Release restored."})
}

// DraftReleaseNotes drafts a release's notes from what the caller may read of
// it, on the AI edition (none: not found); saving them is EditRelease.
// POST /project/{project_uuid}/releases/{release_id}/notes/draft
func DraftReleaseNotes(w http.ResponseWriter, r *http.Request) {
	if !releaseBusiness.DraftsNotes() {
		write(w, r, "DraftReleaseNotes", helpers.ErrNotFound)
		return
	}
	rel, who, ok := manage(w, r)
	if !ok {
		return
	}
	notes, err := releaseBusiness.DraftNotes(r.Context(), who, rel)
	var refused *releaseBusiness.DraftRefusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Code, helpers.Envolope{"msg": refused.Msg})
		return
	}
	if err != nil {
		write(w, r, "DraftReleaseNotes", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"notes": notes}})
}

// GetTaskReleases is a task's release field: the releases of its project it
// is in, those it may go in, and what the caller may change there.
// GET /task/release/{task_uuid}
func GetTaskReleases(w http.ResponseWriter, r *http.Request) {
	_, task, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_uuid"), workAccess.NeedView)
	if !ok {
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	field, err := releaseBusiness.ForTask(r.Context(), who, task)
	if err != nil {
		write(w, r, "GetTaskReleases", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": field})
}

// SetTaskReleases sets the releases a task is in, the whole set: whoever may
// change the task (NeedEditTask, as its cycle), a release already out its
// project's admins only. POST /task/release {task_uuid, release_ids}
func SetTaskReleases(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Task     string   `json:"task_uuid"`
		Releases []string `json:"release_ids"`
	}
	if !httpx.ReadJSON(w, r, 16<<10, &in) {
		return
	}
	_, task, ok := projectaccess.RequireTask(w, r, in.Task, workAccess.NeedEditTask)
	if !ok {
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	field, err := releaseBusiness.SetForTask(r.Context(), who, task, in.Releases)
	if err != nil {
		write(w, r, "SetTaskReleases", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": field})
}
