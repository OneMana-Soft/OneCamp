package business

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A change whose audit row couldn't be written is refused with a 503 the
// client can retry, never the routes' old 400 "Failed to add member to team"
// (V-289: the route half of V-243's "no record, no action").
func TestARefusedAuditRowAnswersRetryLater(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/team/member", nil)
	err := fmt.Errorf("%w: %v", ErrNotRecorded, errors.New("pq: connection refused"))

	if !RefusedUnrecorded(w, r, err, "AddMemberToTeam") {
		t.Fatal("refusedUnrecorded returned false for an error wrapping ErrNotRecorded, so the route would answer its own 400")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the client retries", w.Code)
	}
	var body map[string]interface{}
	if jerr := json.Unmarshal(w.Body.Bytes(), &body); jerr != nil {
		t.Fatalf("body is not JSON: %v (%q)", jerr, w.Body.String())
	}
	msg, _ := body["msg"].(string)
	if !strings.Contains(msg, "couldn't be recorded in the audit log, so it wasn't done") || !strings.Contains(msg, "Try again in a moment.") {
		t.Fatalf("msg = %q, want the audit refusal and the retry-later text", msg)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("the body carries the database error: %q", w.Body.String())
	}
}

// Every other error is the route's to answer: refusedUnrecorded says no and
// writes nothing, so the route's own status and message still reach the client.
func TestAnyOtherErrorIsLeftToTheRoute(t *testing.T) {
	for name, err := range map[string]error{
		"plain error": errors.New("dgraph: team not found"),
		"nil":         nil,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/team/member", nil)
		if RefusedUnrecorded(w, r, err, "AddMemberToTeam") {
			t.Fatalf("%s: refusedUnrecorded returned true for an error that doesn't wrap ErrNotRecorded", name)
		}
		if w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Fatalf("%s: refusedUnrecorded wrote a response (headers %v, body %q); the route answers this one", name, w.Header(), w.Body.String())
		}
	}
}

// Every membership row names its container and person alike.
func TestMembershipRowsNameTheirContainerAlike(t *testing.T) {
	got := fmt.Sprint(MembershipMeta("channel", "c-1", "launch", "u-1"))
	if got != "map[channel_name:launch channel_uuid:c-1 user_uuid:u-1]" {
		t.Fatalf("MembershipMeta = %s", got)
	}
	if got := fmt.Sprint(MembershipMeta("team", "t-1", "", "u-1")); got != "map[team_uuid:t-1 user_uuid:u-1]" {
		t.Fatalf("a nameless team: %s", got)
	}
}
