package Calendar

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
)

// A guest's page answers every link it can't use alike (no oracle): an
// unknown, removed, deleted or long-over link is the same 404. A calendar
// being moved is a 503 to try again, and a server failing is never a 404.
func TestAGuestsPageSaysAlikeEveryLinkItCantUse(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		status int
		msg    string
	}{
		{"gone", business.ErrInvitationGone, http.StatusNotFound, business.ErrInvitationGone.Error()},
		{"moving", business.ErrMoving, http.StatusServiceUnavailable, ""},
		{"a bad answer", &business.EventInputError{}, http.StatusBadRequest, ""},
		{"down", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable, "Your invitation couldn't be opened just now. Try again in a moment."},
	} {
		rec := httptest.NewRecorder()
		writeInvitationError(rec, httptest.NewRequest(http.MethodGet, "/public/rsvp/x", nil), "test", c.err)
		var body struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || (c.msg != "" && body.Msg != c.msg) {
			t.Errorf("%s: %d %q", c.name, rec.Code, body.Msg)
		}
	}
}
