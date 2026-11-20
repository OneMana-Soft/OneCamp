package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The web offers Try again exactly when an AI answer carries Retry-After, so the header follows the words: every msg
// that tells the person to try again carries it, and no other does. This walks every ClientError branch and the
// literal words the AI routes answer with.
func TestTheWordsAndTheRetryAfterAgree(t *testing.T) {
	ctx := context.Background()
	saved := retryNow
	t.Cleanup(func() { retryNow = saved })
	retryNow = func() time.Time { return time.Date(2026, 11, 18, 11, 40, 17, 0, time.UTC) }

	providerRateLimited := FromProvider(fmt.Errorf("groq status 429: slow down: %w", ErrProviderRateLimited))
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"context overflow", fmt.Errorf("prompt: %w", ErrPromptTooLarge), 0},
		{"ollama too old", ErrOllamaUpdateRequired, 0},
		{"AI off", errServiceDisabled, 0},
		{"breaker open", ErrCircuitOpen, 60},
		{"user daily", ErrUserTokenBudgetExceeded, 0},
		{"agent daily", ErrAgentTokenBudgetExceeded, 0},
		{"channel daily", ErrChannelTokenBudgetExceeded, 0},
		{"workspace daily", ErrWorkspaceTokenBudgetExceeded, 0},
		{"too slow", context.DeadlineExceeded, 5},
		{"cancelled", context.Canceled, 0},
		{"limiter, no window", ErrRateLimited, 43},
		{"limiter, its own window", RateLimitedFor(41500 * time.Millisecond), 42},
		{"provider 429", providerRateLimited, 5},
		{"can't reach", FromProvider(errors.New("dial tcp 10.0.0.9:11434: connect: connection refused")), 0},
		{"auth", FromProvider(errors.New("openai status 401: invalid api key")), 0},
		{"model missing", FromProvider(errors.New("ollama status 404: model not found")), 0},
		{"provider, anything else", FromProvider(errors.New("openai status 500: oops")), 5},
		{"no case names it", errors.New("dgraph: txn aborted at 0x1f"), 5},
	} {
		status, words := ClientError(ctx, c.err)
		got := RetryAfterSeconds(c.err, words)
		says := strings.Contains(strings.ToLower(words), "try again")
		if says != (got > 0) {
			t.Errorf("%s: %d %q carries Retry-After %d: the words and the header disagree", c.name, status, words, got)
		}
		if got != c.want {
			t.Errorf("%s: Retry-After %d, want %d (%q)", c.name, got, c.want, words)
		}
		h := http.Header{}
		SetRetryAfter(h, c.err, words)
		if (h.Get("Retry-After") != "") != (c.want > 0) || (c.want > 0 && h.Get("Retry-After") != fmt.Sprint(c.want)) {
			t.Errorf("%s: header %q, want %d", c.name, h.Get("Retry-After"), c.want)
		}
	}

	// The literal words the AI routes answer with, by the same rule.
	for _, words := range []string{
		"OneCamp AI couldn't start the answer. Try again.",
		"That didn't run. Try again in a moment.",
		"Ask AI couldn't start. Try again in a moment.",
		"You've asked AI a lot in the last minute. Try again in a moment.",
	} {
		if got := RetryAfterSeconds(nil, words); got != 5 {
			t.Errorf("%q: Retry-After %d, want 5", words, got)
		}
	}
	for _, words := range []string{
		AIOffWords,
		"That request is gone. It may have been handled already.",
		"Only the person it was proposed to can approve it.",
		"This request expired. Ask again to run it.",
		"This action was already handled",
	} {
		if got := RetryAfterSeconds(nil, words); got != 0 {
			t.Errorf("%q: Retry-After %d, want none", words, got)
		}
	}
	// The limiter's per-minute refusal on the doc routes says try again in its own words, and waits out the window.
	if got := RetryAfterSeconds(RateLimitedFor(9*time.Second), "You've asked AI a lot in the last minute. Try again in a moment."); got != 9 {
		t.Errorf("doc limiter: Retry-After %d, want 9", got)
	}
}

// An error no case names may hold a store's or a library's own text: ClientError answers it in the generic words and
// keeps the text for the log.
func TestClientErrorAnswersAnUnnamedErrorInTheGenericLine(t *testing.T) {
	err := fmt.Errorf("couldn't load the channel: %w", errors.New("dgraph: txn aborted at 0x1f"))
	status, words := ClientError(context.Background(), err)
	if status != http.StatusInternalServerError || words != "The AI model couldn't complete that request. Please try again." {
		t.Errorf("ClientError = %d %q: an unnamed error reached the person", status, words)
	}
	if strings.Contains(words, "dgraph") {
		t.Errorf("the store's text reached the person: %q", words)
	}
}

// The admin page for models and limits is "AI & agents" in the web's nav: every line that sends an admin there names it.
func TestTheWordsNameTheAdminPageByItsNavLabel(t *testing.T) {
	for _, err := range []error{
		errServiceDisabled, ErrUserTokenBudgetExceeded, ErrAgentTokenBudgetExceeded, ErrChannelTokenBudgetExceeded,
		ErrWorkspaceTokenBudgetExceeded, errors.New("connection refused"), errors.New("invalid api key"), errors.New("model not found"),
	} {
		words := FriendlyProviderError(err)
		if !strings.Contains(words, "under Admin > AI & agents.") || strings.Contains(words, "AI Models") {
			t.Errorf("%v: %q doesn't name Admin > AI & agents", err, words)
		}
	}
	if AIOffWords != "AI is turned off for this workspace. An admin can turn it on under Admin > AI & agents." {
		t.Errorf("AIOffWords = %q", AIOffWords)
	}
	if FriendlyProviderError(errServiceDisabled) != AIOffWords {
		t.Errorf("AI off answers %q, not AIOffWords", FriendlyProviderError(errServiceDisabled))
	}
}
