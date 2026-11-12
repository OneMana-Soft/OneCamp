package controller

// Typed links' routes (jira-01 J-H): a task relates to, duplicates or clones
// another. Blocking is not a link but a dependency (POST /task/dependency,
// task_blocked_by: finish to start and the rest), and "blocks" is refused
// here in words that say so. Changes go through the Task Writer
// (business/TaskWriter OpRelation); a removed link is kept, stamped with who
// removed it, and adding it again revives it.

import (
	"encoding/json"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// linkInput is the body of POST /task/link and /task/link/delete.
type linkInput struct {
	Uuid  string `json:"task_uuid"`
	Other string `json:"other_task_uuid"`
	// Kind is relates, duplicates or clones.
	Kind string `json:"kind"`
	// Direction "in" reads the link from the other task: it duplicates (or
	// clones) this one. Anything else: this one duplicates the other.
	Direction string `json:"direction"`
}

// LinkTasks handles POST /task/link {task_uuid, other_task_uuid, kind,
// direction}: the task relates to, duplicates or clones the other. The
// person must be able to change the task and see the other (in another
// project, change tasks there too); a task holds at most business.MaxLinks.
func LinkTasks(w http.ResponseWriter, r *http.Request) { linkChange(w, r, false) }

// UnlinkTasks handles POST /task/link/delete (the same body): the link is
// taken off, kept as removed by the person.
func UnlinkTasks(w http.ResponseWriter, r *http.Request) { linkChange(w, r, true) }

func linkChange(w http.ResponseWriter, r *http.Request, remove bool) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in linkInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That change couldn't be read."})
		return
	}
	taskID, err := uuid.Parse(in.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	value, _ := json.Marshal(map[string]any{"kind": in.Kind, "task": in.Other, "direction": in.Direction, "remove": remove})
	_, _, err = taskWriter.Tasks.Apply(ctx, &userInfo, taskID, []taskWriter.Op{{Kind: taskWriter.OpRelation, Value: value}}, 0)
	writeHierarchyResult(w, r, err, "controllers/linkChange")
}

// TaskLinks handles GET /task/{task_uuid}/links: the task's live links, both
// ways, as the person reads them, with each other task's key. A task they
// may not see on the other end is left out whole, and not counted.
func TaskLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	taskID, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	links, err := business.Links(ctx, taskID, &userInfo)
	if err != nil {
		writeHierarchyResult(w, r, err, "controllers/TaskLinks")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, business.LinkListOf(links))
}
