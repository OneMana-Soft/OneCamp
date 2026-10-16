package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotFoundErrorsAreTheSharedNotFound(t *testing.T) {
	task := NotFound("failed to get dgraph task")
	project := NotFound("failed to get dgraph project")
	if !errors.Is(task, ErrNotFound) || !errors.Is(fmt.Errorf("reading: %w", task), ErrNotFound) {
		t.Fatal("a NotFound error, or one wrapping it, isn't ErrNotFound")
	}
	if task.Error() != "failed to get dgraph task" {
		t.Errorf("a NotFound error lost its words: %q", task.Error())
	}
	// Each stays its own sentinel: a caller matching the task's doesn't catch the project's.
	if errors.Is(project, task) || errors.Is(NotFound("failed to get dgraph task"), task) {
		t.Error("two NotFound sentinels match each other")
	}
	if errors.Is(errors.New("not found"), ErrNotFound) {
		t.Error("an unrelated error with the same words matched ErrNotFound")
	}
}

func TestWriteReadFailureAnswersNotFoundAs404AndAFailedReadAs503(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/task/info/x", nil)
	for _, c := range []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"nothing by that id", NotFound("failed to get dgraph task"), http.StatusNotFound, "Task not found"},
		{"wrapped", fmt.Errorf("business: %w", NotFound("failed to get dgraph task")), http.StatusNotFound, "Task not found"},
		{"a failed read", errors.New("rpc error: Unavailable"), http.StatusServiceUnavailable, "Couldn't load the task. Try again in a moment."},
	} {
		w := httptest.NewRecorder()
		WriteReadFailure(w, r, c.err, "task", "helpers/test")
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.code || body["msg"] != c.msg {
			t.Errorf("%s: got %d %v, want %d %q", c.name, w.Code, body["msg"], c.code, c.msg)
		}
	}
}
