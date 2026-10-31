package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
)

// A task or a sub-task that wasn't made says why in the class the app reads:
// a date that isn't one is the asker's to correct (400), an assignee who
// can't be given work a 409 that names them, and only a failed write is worth
// trying again (503). All three were a 400.
func TestAMadeTaskThatFailedSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code int
		says string
	}{
		{"a due date that isn't one", business.ErrBadDate, http.StatusBadRequest,
			"Couldn't save the dates: a due or start date is an RFC 3339 time, such as 2026-10-23T09:00:00Z."},
		{"a deactivated assignee", helpers.Conflict("Sam Rivera can't be assigned: their account is deactivated.", nil), http.StatusConflict,
			"Sam Rivera can't be assigned: their account is deactivated."},
		{"a store that didn't answer", errors.New("dgraph: deadline exceeded"), http.StatusServiceUnavailable, ""},
	} {
		w := httptest.NewRecorder()
		writeMakeFailure(w, httptest.NewRequest(http.MethodPost, "/task/createTask", nil), c.err, "Couldn't create the task.", "test")
		var body struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.code || (c.says != "" && body.Msg != c.says) {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, body.Msg, c.code, c.says)
		}
	}
}
