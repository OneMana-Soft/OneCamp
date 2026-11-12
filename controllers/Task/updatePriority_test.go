package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// UX-PROJECTS-H1-BE (PROJECTS-14): No priority (a row menu's key 0, a
// selection's No priority) is a priority to set, not a missing field. The
// handler refused "" as required before its own none check could see it, so
// No priority failed with a 400. The body is checked before anything is read:
// a priority that isn't one is refused in words, and only the task and its
// project are required. A task id that isn't one stops each request here,
// before any store is asked.
func TestNoPriorityIsAPriorityToSet(t *testing.T) {
	const project = "7a5c0000-0000-4000-8000-000000000001"
	const pastTheBody = "Failed to parse taskUUID string to uuid"
	for _, c := range []struct {
		name string
		body map[string]any
		msg  string
	}{
		{"no priority", map[string]any{"task_uuid": "not-a-task", "task_project_uuid": project, "task_priority": ""}, pastTheBody},
		{"a priority", map[string]any{"task_uuid": "not-a-task", "task_project_uuid": project, "task_priority": "high"}, pastTheBody},
		{"one that isn't", map[string]any{"task_uuid": "not-a-task", "task_project_uuid": project, "task_priority": "loud"}, "That isn't a priority. Pick urgent, high, medium, low or none."},
		{"no task", map[string]any{"task_project_uuid": project, "task_priority": "high"}, "task_uuid and task_project_uuid are required"},
		{"no project", map[string]any{"task_uuid": "not-a-task", "task_priority": ""}, "task_uuid and task_project_uuid are required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(c.body)
			req := httptest.NewRequest(http.MethodPost, "/task/updateTaskPriority", bytes.NewReader(b))
			me := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x9", Uuid: "7a5c0000-0000-4000-8000-000000000009"}}
			req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, me))
			rec := httptest.NewRecorder()
			UpdateTaskPriority(rec, req)
			var got struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != http.StatusBadRequest || got.Msg != c.msg {
				t.Fatalf("%d %q, want 400 %q", rec.Code, got.Msg, c.msg)
			}
		})
	}
}
