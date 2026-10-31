package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
)

// A late answer to a request that isn't open any more is told so plainly: it
// may have been answered, withdrawn, or closed when its task moved on, and the
// card it was pressed on reads the request again to say which. "Answered
// already" said the wrong thing beside a card that read "Closed: the task
// moved to In review".
func TestALateAnswerIsToldTheRequestIsClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, approvalsBusiness.ErrNotOpen)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusConflict || out["msg"] != "That request is closed now." {
		t.Errorf("a late answer: %d %v", rec.Code, out)
	}
}
