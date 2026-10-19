package Calendar

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
)

// Each way a calendar call fails has its own answer: a store being moved, or
// not on here, is a 503 the web can name by its code, never a fault; a
// change someone else made first is a 409 carrying the event as it is now.
func TestACalendarCallSaysHowItFailed(t *testing.T) {
	current := &business.Event{ID: "e1", Title: "Design review", Sequence: 3}
	for _, c := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"moving", business.ErrMoving, http.StatusServiceUnavailable, "calendar_moving"},
		{"off", business.ErrStoreOff, http.StatusServiceUnavailable, "calendar_off"},
		{"stale", &business.StaleError{Current: current}, http.StatusConflict, ""},
		{"not there", business.ErrEventNotFound, http.StatusNotFound, ""},
		{"not theirs to change", business.ErrOnlyCreatorChanges, http.StatusForbidden, ""},
		{"not readable", &business.EventInputError{}, http.StatusBadRequest, ""},
		{"a yes before answers", business.ErrUnavailable, http.StatusBadRequest, ""},
		{"down", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable, ""},
	} {
		rec := httptest.NewRecorder()
		writeCalendarError(rec, httptest.NewRequest(http.MethodGet, "/calendar/events", nil), "test", c.err)
		var body struct {
			Msg  string          `json:"msg"`
			Code string          `json:"code"`
			Data *business.Event `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body.Code != c.code {
			t.Errorf("%s: %d %q, want %d %q", c.name, rec.Code, body.Code, c.status, c.code)
		}
		if c.name == "stale" && (body.Data == nil || body.Data.Sequence != 3) {
			t.Errorf("a stale change carries %+v", body.Data)
		}
		if c.name == "down" && body.Msg != "Something went wrong. Try again in a moment." {
			t.Errorf("a fault says %q", body.Msg)
		}
	}
	// The /event routes say a move the same way.
	rec := httptest.NewRecorder()
	writeEventError(rec, business.ErrMoving)
	if rec.Code != http.StatusServiceUnavailable || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("the old routes, moving: %d %s", rec.Code, rec.Body.String())
	}
}
