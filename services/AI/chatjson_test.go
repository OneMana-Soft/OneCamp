package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeLLM returns scripted outputs/errors per call, recording how many times
// Chat was invoked. Implements the LLMProvider interface for tests.
type fakeLLM struct {
	outs  []string
	errs  []error
	calls int
}

func (f *fakeLLM) Chat(_ context.Context, _ []ChatMessage, _ ChatOptions) (string, error) {
	i := f.calls
	f.calls++
	var out string
	if i < len(f.outs) {
		out = f.outs[i]
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	return out, err
}

func (f *fakeLLM) ChatStream(_ context.Context, _ []ChatMessage, _ ChatOptions) (<-chan StreamChunk, error) {
	return nil, errors.New("not used")
}
func (f *fakeLLM) ProviderName() ProviderType { return "fake" }

func validJSON(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "{") }

func TestChatJSONWithRetry_FirstValid(t *testing.T) {
	f := &fakeLLM{outs: []string{`{"ok":true}`}}
	out, err := ChatJSONWithRetry(context.Background(), f, nil, "sys", "user", ChatOptions{}, validJSON)
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if f.calls != 1 {
		t.Fatalf("valid first output must not retry; calls=%d", f.calls)
	}
}

func TestChatJSONWithRetry_RetryRecovers(t *testing.T) {
	// First output wrapped in prose (invalid), retry returns clean JSON.
	f := &fakeLLM{outs: []string{"Sure! ```json\n{bad", `{"ok":true}`}}
	out, err := ChatJSONWithRetry(context.Background(), f, nil, "sys", "user", ChatOptions{}, validJSON)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if f.calls != 2 {
		t.Fatalf("invalid first output must trigger one retry; calls=%d", f.calls)
	}
	if out != `{"ok":true}` {
		t.Fatalf("expected the corrected output, got %q", out)
	}
}

func TestChatJSONWithRetry_FirstErrorReturns(t *testing.T) {
	f := &fakeLLM{errs: []error{errors.New("boom")}}
	_, err := ChatJSONWithRetry(context.Background(), f, nil, "sys", "user", ChatOptions{}, validJSON)
	if err == nil {
		t.Fatal("a transport error on the first call must be returned")
	}
	if f.calls != 1 {
		t.Fatalf("must not retry after a transport error; calls=%d", f.calls)
	}
}

func TestChatJSONWithRetry_RetryErrorFallsBack(t *testing.T) {
	// First output invalid, retry transport fails → fall back to first output.
	f := &fakeLLM{outs: []string{"prose only", ""}, errs: []error{nil, errors.New("net")}}
	out, err := ChatJSONWithRetry(context.Background(), f, nil, "sys", "user", ChatOptions{}, validJSON)
	if err != nil {
		t.Fatalf("retry transport error should fall back, not error; got %v", err)
	}
	if out != "prose only" {
		t.Fatalf("expected fallback to first output, got %q", out)
	}
}
