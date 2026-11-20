package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// answered reads an answer's status, msg, err and Retry-After.
func answered(t *testing.T, w *httptest.ResponseRecorder) (int, string, any, string) {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	msg, _ := body["msg"].(string)
	return w.Code, msg, body["err"], w.Header().Get("Retry-After")
}

// Catch-up and the three Summarize routes answer ClientError's words in msg (the err key keeps them too), with
// Retry-After exactly when the words say try again.
func TestACatchUpOrSummaryFailureSaysWhatHappenedInMsg(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		msg    string
		retry  string
	}{
		{ai.ErrCircuitOpen, 503, "The AI provider isn't answering right now. Please try again in a minute.", "60"},
		{ai.RateLimitedFor(30 * time.Second), 429, "You're asking the AI faster than this workspace allows. Please wait a minute and try again.", "30"},
		{ai.ErrUserTokenBudgetExceeded, 429, "You've reached your daily AI usage limit. It resets tomorrow, or an admin can raise your limit under Admin > AI & agents.", ""},
		// ClientError's 504 leaves as 503: WriteJSON's edgeSafeStatus, so Cloudflare shows the words, not a network error.
		{context.DeadlineExceeded, 503, "The AI model took too long to respond. If it's a local model it may still be loading - try again in a moment, or pick a faster model in the assistant's model menu.", "5"},
		{fmt.Errorf("catch-up: %w", errors.New("dgraph: txn aborted")), 500, "The AI model couldn't complete that request. Please try again.", "5"},
	} {
		w := httptest.NewRecorder()
		answerAIFailure(w, context.Background(), c.err)
		status, msg, errWords, retry := answered(t, w)
		if status != c.status || msg != c.msg || errWords != c.msg || retry != c.retry {
			t.Errorf("%v: %d msg=%q err=%v Retry-After=%q, want %d %q %q", c.err, status, msg, errWords, retry, c.status, c.msg, c.retry)
		}
	}
}

func TestAPendingActionRefusalSaysWhatToDo(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		msg    string
		retry  string
	}{
		{business.ErrPendingActionNotFound, 404, "That request is gone. It may have been handled already.", ""},
		{business.ErrPendingActionForbidden, 403, "Only the person it was proposed to can approve or reject it.", ""},
		{business.ErrPendingActionResolved, 409, "This action was already handled", ""},
		{business.ErrPendingActionExpired, 410, "This request expired. Ask again to run it.", ""},
		{errors.New("postgres: connection reset"), 500, "That didn't run. Try again in a moment.", "5"},
	} {
		w := httptest.NewRecorder()
		writePendingActionError(w, c.err, nil)
		status, msg, _, retry := answered(t, w)
		if status != c.status || msg != c.msg || retry != c.retry {
			t.Errorf("%v: %d %q Retry-After=%q, want %d %q %q", c.err, status, msg, retry, c.status, c.msg, c.retry)
		}
	}
}

func TestAIOffAndAStreamThatCantStartAnswerInWords(t *testing.T) {
	w := httptest.NewRecorder()
	answerAIOff(w)
	if status, msg, _, retry := answered(t, w); status != 503 || msg != "AI is turned off for this workspace. An admin can turn it on under Admin > AI & agents." || retry != "" {
		t.Errorf("AI off: %d %q Retry-After=%q", status, msg, retry)
	}
	w = httptest.NewRecorder()
	answerNoStream(w)
	if status, msg, _, retry := answered(t, w); status != 500 || msg != "OneCamp AI couldn't start the answer. Try again." || retry != "5" {
		t.Errorf("no stream: %d %q Retry-After=%q", status, msg, retry)
	}
	// The route itself, with no AI service.
	w = httptest.NewRecorder()
	AskAIStream(w, docAIRequest(t, "/ai/ask/stream", map[string]string{"question": "When is the launch?"}))
	if status, msg, _, retry := answered(t, w); status != 503 || msg != ai.AIOffWords || retry != "" {
		t.Errorf("ask with AI off: %d %q Retry-After=%q", status, msg, retry)
	}
}

func TestAskAIThatCantStartSaysAskAIAndTryAgain(t *testing.T) {
	w := httptest.NewRecorder()
	writeDocAIError(w, context.Background(), errors.New("read doc: dgraph: txn aborted"))
	if status, msg, _, retry := answered(t, w); status != http.StatusInternalServerError || msg != "Ask AI couldn't start. Try again in a moment." || retry != "5" {
		t.Errorf("prepare failed: %d %q Retry-After=%q", status, msg, retry)
	}
	w = httptest.NewRecorder()
	writeDocAIError(w, context.Background(), &docAIError{status: http.StatusServiceUnavailable, msg: ai.AIOffWords})
	if status, msg, _, retry := answered(t, w); status != 503 || msg != "AI is turned off for this workspace. An admin can turn it on under Admin > AI & agents." || retry != "" {
		t.Errorf("AI off after the gate: %d %q Retry-After=%q", status, msg, retry)
	}
	w = httptest.NewRecorder()
	breakerRefusal(w, ai.ErrCircuitOpen)
	if status, _, _, retry := answered(t, w); status != 503 || retry != "60" {
		t.Errorf("breaker open: %d Retry-After=%q", status, retry)
	}
}

// The doc AI gate: AI off carries no Retry-After; the per-minute refusal waits out the limiter's own window.
func TestTheDocGateSaysWhenToAskAgain(t *testing.T) {
	for _, c := range []struct {
		rt     fakeRuntime
		status int
		retry  string
	}{
		{fakeRuntime{off: true}, 503, ""},
		{fakeRuntime{limited: true}, 429, "42"},
	} {
		c.rt.model = &streamModel{chunks: []string{"x"}}
		useRuntime(t, c.rt)
		w := httptest.NewRecorder()
		DocAICompleteStream(w, docAIRequest(t, "/ai/doc/complete/stream", DocAIRequest{Action: "improve", Text: "x"}))
		if status, msg, _, retry := answered(t, w); status != c.status || retry != c.retry {
			t.Errorf("%+v: %d %q Retry-After=%q, want %d %q", c.rt, status, msg, retry, c.status, c.retry)
		}
	}
}
