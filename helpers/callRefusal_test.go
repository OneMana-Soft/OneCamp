package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A refusal answers 403 with its own code and words, even when it was
// wrapped on its way up, and anything else is left to the caller.
func TestACallRefusalIsAnsweredInItsOwnWords(t *testing.T) {
	locked := &CallRefusal{Code: "call_locked", Msg: "This call is locked."}
	rec := httptest.NewRecorder()
	if !WriteCallRefusal(rec, fmt.Errorf("minting a token: %w", locked)) {
		t.Fatal("a wrapped refusal wasn't answered")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "call_locked" || body["msg"] != "This call is locked." {
		t.Fatalf("answered %v", body)
	}

	other := httptest.NewRecorder()
	if WriteCallRefusal(other, errors.New("the call server is down")) {
		t.Fatal("an ordinary error was answered as a refusal")
	}
	if other.Body.Len() != 0 {
		t.Fatalf("an ordinary error wrote %q", other.Body.String())
	}
}

// A refusal that passes (the call couldn't be checked just now) is answered
// with its own status, a 503 a page tries again, in its own words.
func TestARefusalThatPassesIsAnsweredToTryAgain(t *testing.T) {
	unchecked := &CallRefusal{Code: "call_unchecked", Msg: "We couldn't check the call just now. Try again in a moment.", Status: http.StatusServiceUnavailable}
	rec := httptest.NewRecorder()
	if !WriteCallRefusal(rec, fmt.Errorf("minting a token: %w", unchecked)) || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "call_unchecked" {
		t.Fatalf("answered %v %v", body, err)
	}
}
