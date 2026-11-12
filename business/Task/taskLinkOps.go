package business

// A task's links on the public API (jira-01 J-H), declared once as an op:
// GET /v1/tasks/{id}/links, and in the AI edition the governed MCP tool
// tasks_links (business/MCPServer PublishOps). Both serve through Links, the
// links panel's read, as the token's member: a link to a task they can't see
// names nothing on any surface, and every surface answers the one LinkList.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// LinkList is a task's links as every surface answers them: the panel's
// route, the public API and MCP.
type LinkList struct {
	Links []TaskLink `json:"links"`
	Count int        `json:"count"`
}

// LinkListOf is links as a LinkList: an empty list, never null.
func LinkListOf(links []TaskLink) LinkList {
	if links == nil {
		links = []TaskLink{}
	}
	return LinkList{Links: links, Count: len(links)}
}

// APIOps are the tasks' ops on the public API, registered beside the tasks
// resource's routes (controllers/V1).
func APIOps() []v1api.Op {
	return []v1api.Op{{
		Name:      "tasks.links",
		Summary:   "A task's links (relates to, duplicates, clones), both ways, each with the other task as you may see it",
		Method:    http.MethodGet,
		Path:      "/tasks/{id}/links",
		Scope:     apiTokenBusiness.ScopeTasksRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","maxLength":64,"description":"The task's id."}},"required":["id"]}`),
		Reach:     taskReach,
		Serve:     serveLinks,
	}}
}

// taskReach is the task an op's "id" names: an app is held to it.
func taskReach(args map[string]any) (string, string) {
	id, _ := args["id"].(string)
	return "task", id
}

// errNoTask is a task the caller can't read, in the API's words: one that
// isn't there and one kept from them answer alike.
var errNoTask = v1api.Errorf(http.StatusNotFound, "not_found", "", "task not found or you don't have access")

// serveLinks reads the task's links as the token's member. Its id may be the
// task's key (Q4-12), as on the API's other task routes.
func serveLinks(ctx context.Context, call v1api.Call) (any, error) {
	who, _ := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	ref, _ := call.Args["id"].(string)
	id, err := taskKeys.Ref(ctx, ref)
	switch {
	case errors.Is(err, taskKeys.ErrNoTask) || id == uuid.Nil && err == nil:
		return nil, errNoTask
	case err != nil:
		return nil, v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "try again", "couldn't look the task up")
	}
	links, err := Links(ctx, id, &who)
	switch {
	case errors.Is(err, ErrTaskNotVisible):
		return nil, errNoTask
	case err != nil:
		return nil, v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "try again", "couldn't read the task's links")
	}
	return LinkListOf(links), nil
}
