package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
)

// A decision triage refuses is said in words with its own code, never as a
// fault to retry: an accept the project's priority rule holds back
// (projects.triage.priority) says what to do; a move the project's status
// rules hold back says why, as a status change anywhere does
// (projectaccess.RefuseStatus: 409, "approval" with the request it became, or
// "status_rule"); a status the project doesn't have is the caller's to fix.
func TestADecisionTriageRefusesIsSaidInWords(t *testing.T) {
	cases := []struct {
		err  error
		code int
		msg  string
		more map[string]any
	}{
		{triageBusiness.ErrNeedsPriority, http.StatusConflict, "Give it a priority before accepting it.", nil},
		{fmt.Errorf("%w: a decision is accept, duplicate, decline or snooze", triageBusiness.ErrBadChoice), http.StatusBadRequest, "a decision is accept, duplicate, decline or snooze", nil},
		{triageBusiness.ErrNotWaiting, http.StatusConflict, "That has been decided already.", nil},
		{fmt.Errorf("task writer: %w", &taskBusiness.StatusRefusal{Why: "Done needs a review first.", ApprovalID: "ap-1"}), http.StatusConflict,
			"Done needs a review first.", map[string]any{"code": "approval", "approval_id": "ap-1"}},
		{&taskBusiness.StatusRefusal{Why: "Nothing moves to Canceled here."}, http.StatusConflict,
			"Nothing moves to Canceled here.", map[string]any{"code": "status_rule"}},
		{fmt.Errorf("task writer: %w", taskStatusBusiness.ErrUnknownStatus), http.StatusBadRequest, "That is not a status in this project.", nil},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, httptest.NewRequest(http.MethodPost, "/task/triage/x/decide", nil), c.err)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != c.code || out["msg"] != c.msg {
			t.Errorf("%v: %d %v, want %d %q", c.err, rec.Code, out, c.code, c.msg)
		}
		for k, v := range c.more {
			if out[k] != v {
				t.Errorf("%v: %s is %v, want %v", c.err, k, out[k], v)
			}
		}
	}
}
