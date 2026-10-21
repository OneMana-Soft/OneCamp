package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A clash is the person's to sort out, so it is a 409 in words written for
// them, from reads and writes alike; any other failed write is a 503 with
// what couldn't be done.
func TestFailuresAnswerInSentences(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/task/moveTask", nil)
	taken := Conflict(`Another booking page already uses the address "standup". Pick another.`, errors.New("slug taken"))
	for _, c := range []struct {
		name  string
		write func(w http.ResponseWriter, err error)
		err   error
		code  int
		msg   string
	}{
		{"a clash, written", writeFailure("Couldn't save the page."), taken, http.StatusConflict, `Another booking page already uses the address "standup". Pick another.`},
		{"a clash, wrapped", writeFailure("Couldn't save the page."), fmt.Errorf("business: %w", taken), http.StatusConflict, `Another booking page already uses the address "standup". Pick another.`},
		{"lost to a change made at the same moment", writeFailure("Couldn't move the task."), Conflict(ConcurrentChangeText, errors.New("Transaction has been aborted. Please retry")), http.StatusConflict, ConcurrentChangeText},
		{"any other failed write", writeFailure("Couldn't move the task."), errors.New("rpc error: Unavailable"), http.StatusServiceUnavailable, "Couldn't move the task. Try again in a moment."},
		{"a clash met while reading", func(w http.ResponseWriter, err error) { WriteReadFailure(w, r, err, "page", "helpers/test") }, taken, http.StatusConflict, `Another booking page already uses the address "standup". Pick another.`},
	} {
		w := httptest.NewRecorder()
		c.write(w, c.err)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.code || body["msg"] != c.msg {
			t.Errorf("%s: got %d %v, want %d %q", c.name, w.Code, body["msg"], c.code, c.msg)
		}
	}
	if !errors.Is(taken, ErrConflict) || errors.Is(errors.New("conflict"), ErrConflict) {
		t.Error("a Conflict isn't ErrConflict, or an unrelated error is")
	}
	cause := errors.New("Transaction has been aborted. Please retry")
	if !errors.Is(Conflict(ConcurrentChangeText, cause), cause) {
		t.Error("a Conflict hides the error that found it")
	}
}

func writeFailure(couldnt string) func(w http.ResponseWriter, err error) {
	return func(w http.ResponseWriter, err error) {
		WriteWriteFailure(w, httptest.NewRequest(http.MethodPost, "/", nil), err, couldnt, "helpers/test")
	}
}

// The duplicate a handler names is the one it knows: a unique violation on
// another constraint of the same row isn't "that address is taken".
func TestUniqueViolationOnNamesTheConstraint(t *testing.T) {
	slug := &pgconn.PgError{Code: "23505", ConstraintName: "booking_pages_slug_key"}
	other := &pgconn.PgError{Code: "23505", ConstraintName: "booking_pages_pkey"}
	if !IsUniqueViolationOn(fmt.Errorf("save: %w", slug), "booking_pages_slug_key") {
		t.Error("the slug's own violation wasn't recognised")
	}
	if IsUniqueViolationOn(other, "booking_pages_slug_key") {
		t.Error("another constraint's violation was taken for the slug's")
	}
	if IsUniqueViolationOn(&pgconn.PgError{Code: "23503", ConstraintName: "booking_pages_slug_key"}, "booking_pages_slug_key") {
		t.Error("a foreign-key failure was taken for a duplicate")
	}
	if !IsUniqueViolationOn(uniqueViolation(), "channels_ch_name_key") {
		t.Error("lib/pq's violation wasn't recognised")
	}
}
