//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// A task tool's answer names the task by its key, as Linear names an issue
// wherever it's mentioned (linear-02): the API's create and status answers
// and the agents' (and so the MCP server's) create, status, assign and due
// answers carry "key" and say it before the task's name.
func TestATaskToolsAnswerNamesTheTasksKey(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	projectKey, err := taskKeys.Ensure(ctx, uuid.MustParse(w.project), "Launch")
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		Message string            `json:"message"`
		Data    map[string]string `json:"data"`
	}
	// v1 runs one of the API's handlers as ana's token, with the task's id or
	// key in the path.
	v1 := func(h http.HandlerFunc, id string, body map[string]string) answer {
		t.Helper()
		raw, _ := json.Marshal(body)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		c := helpers.WithActsThroughToken(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rc), helpers.UserInfoContextKey, ana))
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
		var out answer
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	named := func(what string, a answer, name string) string {
		t.Helper()
		key := a.Data["key"]
		if !strings.HasPrefix(key, projectKey+"-") || !strings.Contains(a.Message, key+` "`+name+`"`) {
			t.Errorf("%s: key %q, message %q", what, key, a.Message)
		}
		return key
	}

	made := v1(v1Controller.CreateTaskV1, "", map[string]string{"task_name": "Book the room", "project_uuid": w.project})
	key := named("the API's create", made, "Book the room")
	if moved := v1(v1Controller.UpdateTaskStatusV1, key, map[string]string{"status": "inProgress"}); named("the API's status", moved, "Book the room") != key {
		t.Errorf("the status answer names another task: %+v", moved)
	}

	// tool runs an agent's tool as ana through its executor, as the MCP bridge
	// does (handlerFor); the AI service itself is off here.
	aiBusiness.RegisterToolExecutors()
	tool := func(name string, params map[string]string) answer {
		t.Helper()
		exec, ok := ai.GetExecutor(name)
		if !ok {
			t.Fatalf("%s isn't registered", name)
		}
		msg, data, err := exec(ctx, ai.ProposedAction{ToolName: name, Params: params}, ana.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return answer{Message: msg, Data: data}
	}
	other := named("an agent's create", tool("create_task", map[string]string{"task_name": "Print the badges", "project_uuid": w.project}), "Print the badges")
	if other == key {
		t.Errorf("two tasks share the key %s", key)
	}
	named("an agent's status", tool("update_task_status", map[string]string{"task_uuid": other, "status": "done"}), "Print the badges")
	named("an agent's assign", tool("assign_task", map[string]string{"task_uuid": key, "assignee_uuid": w.people["ben"].String()}), "Book the room")
	named("an agent's due date", tool("set_task_due_date", map[string]string{"task_uuid": key, "due_date": "2026-11-02T09:00:00Z"}), "Book the room")
}
