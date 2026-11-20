package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// Doc AI's answers that say try again carry Retry-After, and the daily budget's doesn't (CR-437 F1, F2).
func TestADocAIAnswerThatSaysTryAgainSaysWhen(t *testing.T) {
	w := httptest.NewRecorder()
	writeDocAIError(w, context.Background(), &docAIError{status: http.StatusServiceUnavailable,
		msg: "The doc's text couldn't be read just now. Try again in a moment."})
	if status, msg, _, retry := answered(t, w); status != 503 || retry != "5" {
		t.Errorf("doc text unreadable: %d %q Retry-After=%q, want 503 \"5\"", status, msg, retry)
	}

	useRuntime(t, fakeRuntime{model: &streamModel{chunks: []string{""}}})
	w = httptest.NewRecorder()
	DocAIComplete(w, docAIRequest(t, "/ai/doc/complete", DocAIRequest{Action: "improve", Text: "x"}))
	if status, msg, _, retry := answered(t, w); status != 503 || msg != "The model sent back nothing. Try again, or pick another model." || retry != "5" {
		t.Errorf("empty answer: %d %q Retry-After=%q, want 503 \"5\"", status, msg, retry)
	}

	for _, c := range []struct {
		err   error
		retry string
	}{
		{context.DeadlineExceeded, "5"},
		{errors.New("provider said no"), "5"},
		{ai.ErrUserTokenBudgetExceeded, ""},
	} {
		w = httptest.NewRecorder()
		answerDocModelFailure(w, c.err)
		if status, msg, _, retry := answered(t, w); status != 503 || retry != c.retry {
			t.Errorf("model failure %v: %d %q Retry-After=%q, want 503 %q", c.err, status, msg, retry, c.retry)
		}
	}
}

// The doc AI's own per-minute check (applied edits) waits out the limiter's window (CR-437 F4).
func TestTheAppliedEditsLimitSaysWhenToAskAgain(t *testing.T) {
	useRuntime(t, fakeRuntime{limited: true, model: &streamModel{}})
	w := httptest.NewRecorder()
	if limitedDocAI(w, docAIRequest(t, "/ai/doc/applied", nil), "u") {
		t.Fatal("a limited person was let through")
	}
	if status, _, _, retry := answered(t, w); status != 429 || retry != "42" {
		t.Errorf("applied-edits limit: %d Retry-After=%q, want 429 \"42\"", status, retry)
	}
}

// The in-call ask's breaker and limiter refusals say when to ask again (CR-437 F4, mutant M9).
func TestTheInCallPreCheckSaysWhenToAskAgain(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		retry  string
	}{
		{ai.ErrCircuitOpen, 503, "60"},
		{ai.RateLimitedFor(42 * time.Second), 429, "42"},
	} {
		w := httptest.NewRecorder()
		answerPreCheck(w, context.Background(), c.err)
		if status, msg, _, retry := answered(t, w); status != c.status || retry != c.retry {
			t.Errorf("%v: %d %q Retry-After=%q, want %d %q", c.err, status, msg, retry, c.status, c.retry)
		}
	}
}
