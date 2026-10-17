package entityLinkController

// Connected work (docs programme C1): what live chips show, who an "@" in a
// doc reaches, and to-dos that become tasks. Mounted by router/docsConnected.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/EntityLink"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
)

// ids is whether every one is a uuid.
func ids(all ...string) bool {
	for _, id := range all {
		if !helpers.IsCanonicalUUID(id) {
			return false
		}
	}
	return true
}

// ReadEntities POST /entities {refs: [{kind, id}]}: what each chip shows the
// reader, with access decided for every one.
func ReadEntities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in struct {
		Refs []business.Ref `json:"refs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	got, err := business.ReadEntities(ctx, who, in.Refs)
	if errors.Is(err, business.ErrTooMany) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("Ask for at most %d things at once.", business.MaxRefs)})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ReadEntities err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read what these link to just now. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Read", "data": got})
}

// MentionAccess POST /doc/mentionAccess {doc_uuid, user_uuids}: which of the
// people the "@" list offers can't open the doc being written, and whether
// the writer may share it with them.
func MentionAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID   string   `json:"doc_uuid"`
		UserUUIDs []string `json:"user_uuids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !ids(in.DocUUID) || !ids(in.UserUUIDs...) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name the doc and the people."})
		return
	}
	got, err := business.MentionAccessFor(ctx, who, in.DocUUID, in.UserUUIDs)
	switch {
	case errors.Is(err, business.ErrTooManyPeople):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("Ask about at most %d people at once.", business.MaxMentionChecks)})
	case errors.Is(err, business.ErrNoDoc):
		helpers.WriteDocNotFound(w)
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/MentionAccess err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't check who can open this doc just now. Try again."})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Read", "data": got})
	}
}

// What someone who doesn't run the project is told: making a task from a
// to-do, or ticking one that is a task.
const (
	notAdminToMake = "Only the project's admins can make its tasks. Ask one of them, or pick a project you run."
	notAdminToTick = "Only the project's admins can change this task. Ask one of them to change it."
)

// todoRefusal answers a refusal of the to-do and task sync, or false for an
// error that isn't one; notAdmin is what a project's non-admin is told.
func todoRefusal(w http.ResponseWriter, err error, notAdmin string) bool {
	switch {
	case errors.Is(err, taskBusiness.ErrDocNotEditable):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't edit this doc, so its to-dos stay as they are."})
	case errors.Is(err, taskBusiness.ErrNotProjectAdmin):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": notAdmin})
	case errors.Is(err, taskBusiness.ErrEmptyTodo):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Write the to-do first, then make it a task."})
	case errors.Is(err, taskBusiness.ErrTodoNotTied):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That to-do isn't a task any more."})
	case errors.Is(err, taskBusiness.ErrTaskNotVisible), errors.Is(err, taskBusiness.ErrTaskDeleted):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That task was deleted, or you can't see it."})
	default:
		return false
	}
	return true
}

// TodoToTask POST /doc/todoTask {doc_uuid, project_uuid, title}: makes a task
// from a to-do in a doc, tied to it, and answers its id.
func TodoToTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, _ := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID     string `json:"doc_uuid"`
		ProjectUUID string `json:"project_uuid"`
		Title       string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !ids(in.DocUUID, in.ProjectUUID) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name the doc and the project."})
		return
	}
	if len([]rune(in.Title)) > 500 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task's name can be up to 500 characters. Shorten the to-do first."})
		return
	}
	task, err := taskBusiness.TodoToTask(ctx, &userInfo, in.DocUUID, in.ProjectUUID, in.Title)
	if todoRefusal(w, err, notAdminToMake) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/TodoToTask err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't make the task just now. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Made a task", "data": map[string]string{"task_uuid": task.String()}})
}

// CheckTodo POST /doc/todoTask/check {doc_uuid, task_uuid, done}: a tick or an
// untick of a to-do that is a task, made to the task.
func CheckTodo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, _ := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID  string `json:"doc_uuid"`
		TaskUUID string `json:"task_uuid"`
		Done     bool   `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !ids(in.DocUUID, in.TaskUUID) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name the doc and the task."})
		return
	}
	err := taskBusiness.SetTodoDone(ctx, &userInfo, in.DocUUID, in.TaskUUID, in.Done)
	if todoRefusal(w, err, notAdminToTick) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CheckTodo err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't change the task just now. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Changed the task"})
}
