package controllers

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// A task's links are on the public API as one op (jira-01 J-H): GET
// /v1/tasks/{id}/links, a read under tasks:read, held to the task an app was
// added to. The same declaration is MCP's tool (business/MCPServer PublishOps),
// so the two read through one function and answer alike.
func TestATasksLinksAreOneReadOp(t *testing.T) {
	var got *v1api.Op
	for _, op := range v1api.Ops() {
		if op.Name == "tasks.links" {
			o := op
			got = &o
		}
	}
	if got == nil {
		t.Fatal("no op tasks.links: the public API can't read a task's links")
	}
	if got.Method != http.MethodGet || got.Path != "/tasks/{id}/links" || got.Scope != apiTokenBusiness.ScopeTasksRead ||
		!got.Behaviour.ReadOnly || !got.Behaviour.Idempotent {
		t.Errorf("tasks.links is %s %s under %q (%+v), want a read: GET /tasks/{id}/links under tasks:read", got.Method, got.Path, got.Scope, got.Behaviour)
	}
	id := uuid.NewString()
	if got.Reach == nil {
		t.Fatal("tasks.links names no task an app is held to")
	}
	if kind, ref := got.Reach(map[string]any{"id": id}); kind != "task" || ref != id {
		t.Errorf("tasks.links reaches %s %q, want task %s", kind, ref, id)
	}
}
