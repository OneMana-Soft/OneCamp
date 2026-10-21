package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PR2 (createTasks): many tasks made in one request, for a whiteboard's notes
// or an assistant's proposals, at most 100, each named; a request that breaks
// those is refused whole before anything is read.
func TestMakingManyTasksHasLimits(t *testing.T) {
	for name, c := range map[string]struct {
		body, msg string
	}{
		"unreadable":   {`{`, "That request couldn't be read."},
		"no project":   {`{"tasks":[{"task_name":"a"}]}`, "Say which project the tasks go in."},
		"none":         {`{"project_uuid":"7a5c0000-0000-4000-8000-000000000001","tasks":[]}`, "Name at least one task."},
		"unnamed":      {`{"project_uuid":"7a5c0000-0000-4000-8000-000000000001","tasks":[{"task_name":"  "}]}`, "Every task needs a name."},
		"too many":     {`{"project_uuid":"7a5c0000-0000-4000-8000-000000000001","tasks":[` + strings.Repeat(`{"task_name":"a"},`, 100) + `{"task_name":"a"}]}`, "Make at most 100 tasks at a time."},
		"bad priority": {`{"project_uuid":"7a5c0000-0000-4000-8000-000000000001","tasks":[{"task_name":"a","task_priority":"loud"}]}`, "\"loud\" isn't a priority."},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			CreateTasks(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body)))
			var got struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != http.StatusBadRequest || got.Msg != c.msg {
				t.Fatalf("%d %s, want 400 %q", rec.Code, rec.Body.String(), c.msg)
			}
		})
	}
}
