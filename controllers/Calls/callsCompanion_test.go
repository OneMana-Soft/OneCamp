package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
)

// meet-04. A companion token's refusals are answered in their own words and
// status, never as a server error: someone who may not be in the call is
// 403 not_in_call, a call with nobody in its own room is 409
// call_not_started, and Call Control's own refusals keep theirs.
func TestACompanionsRefusalsAreSaidInTheirWords(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{liveKitBusiness.ErrNoCompanionHere, http.StatusForbidden, "not_in_call"},
		{liveKitBusiness.ErrNobodyToAccompany, http.StatusConflict, "call_not_started"},
		{callControlBusiness.ErrThroughToken, callControlBusiness.ErrThroughToken.Status, callControlBusiness.ErrThroughToken.Code},
		{callControlBusiness.ErrNotInCall, http.StatusForbidden, "not_in_call"},
	} {
		rec := httptest.NewRecorder()
		writeRoomToken(rec, httptest.NewRequest(http.MethodPost, "/calls/s/companion-token", nil), callControlBusiness.RoomToken{}, c.err)
		var answer struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		if rec.Code != c.status || answer.Code != c.code || answer.Msg == "" {
			t.Errorf("%v: answered %d %q %q, want %d %q in words", c.err, rec.Code, answer.Code, answer.Msg, c.status, c.code)
		}
	}
}
