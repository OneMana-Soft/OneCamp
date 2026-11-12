package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
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

// When the host refuses an empty answer in JSON mode (400 json_validate_failed with an empty failed_generation,
// or one of whitespace only), the model wrote nothing: it spent its whole budget thinking. ChatJSONWithRetry
// asks once more at the least thinking the model's family takes, and returns that answer; every call's result
// is in the breaker. Here the admin's thinking switch is on, so the first call is sent no effort, as the demo's
// were (V-811b).
func TestChatJSONWithRetry_AnEmptyJSONRefusalIsAskedOnceMoreThinkingLeast(t *testing.T) {
	refusals := map[string]string{
		"an empty failed_generation":             groqRefusal,
		"a failed_generation of whitespace only": strings.Replace(groqRefusal, `"failed_generation":""`, `"failed_generation":" \n "`, 1),
	}
	for _, model := range []string{"gpt-oss-20b", "openai/gpt-oss-20b"} {
		for name, refusal := range refusals {
			t.Run(model+"/"+name, func(t *testing.T) {
				host := newGroqHost(t)
				host.mu.Lock()
				host.refusal = refusal
				host.mu.Unlock()
				p := NewOpenAICompatibleProviderWithOpts("k", model, "", host.URL+"/groq/openai/v1", 0, ProviderOpenAICompatible, ProviderOptions{})
				p.reasoning = true
				cb := NewCircuitBreaker(5, time.Minute)
				out, err := ChatJSONWithRetry(context.Background(), p, cb, "sys", "plan my week", ChatOptions{MaxTokens: 460}, validJSON)
				if err != nil || out != `{"plan":[]}` {
					t.Fatalf("out=%q err=%v", out, err)
				}
				if got := host.sentEfforts(); strings.Join(got, ",") != ",low" {
					t.Errorf("efforts sent %q, want the endpoint's default then low", got)
				}
				if cb.lastFailure.IsZero() || cb.failures != 0 || cb.State() != "closed" {
					t.Errorf("the breaker holds both calls (a failure, then a success): failures=%d last=%v state=%s", cb.failures, cb.lastFailure, cb.State())
				}
			})
		}
	}
}

// The retry is made at most once, and only when it is another request: a call at the least thinking the
// model's family takes on this host differs from the call that was sent. Otherwise it would send the same bytes
// for the same refusal, a second wait, a second spend of the asker's rate limit and a second breaker failure,
// and the breaker opens at 5 (V-811c). The model here writes nothing at every effort, so each case's last call
// is refused and that refusal returned, with every call in the breaker.
func TestChatJSONWithRetry_AnEmptyJSONRefusalIsRetriedAtMostOnce(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		thinking bool
		least    bool   // the caller's own LeastThinking
		efforts  string // the reasoning_effort of each call, "-" for none
	}{
		{"gpt-oss with thinking on is asked once more, at low", "openai/gpt-oss-20b", true, false, "- low"},
		{"gpt-oss with thinking off already went at low", "openai/gpt-oss-20b", false, false, "low"},
		{"a caller that asked to think least", "openai/gpt-oss-20b", true, true, "low"},
		{"a family that takes no effort, with thinking on", "deepseek-r1-distill-llama-70b", true, false, "-"},
		{"a model that doesn't think", "llama-3.3-70b-versatile", false, false, "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := newGroqHost(t)
			host.mu.Lock()
			host.thinksOut = true
			host.mu.Unlock()
			p := NewOpenAICompatibleProviderWithOpts("k", c.model, "", host.URL+"/groq/openai/v1", 0, ProviderOpenAICompatible, ProviderOptions{})
			p.reasoning = c.thinking
			cb := NewCircuitBreaker(5, time.Minute)
			_, err := ChatJSONWithRetry(context.Background(), p, cb, "sys", "plan my week", ChatOptions{MaxTokens: 460, LeastThinking: c.least}, validJSON)
			if err == nil || !strings.Contains(err.Error(), "json_validate_failed") {
				t.Fatalf("the host's refusal is returned: %v", err)
			}
			got := host.sentEfforts()
			for i := range got {
				if got[i] == "" {
					got[i] = "-"
				}
			}
			if strings.Join(got, " ") != c.efforts {
				t.Errorf("efforts sent %q, want %q", strings.Join(got, " "), c.efforts)
			}
			if cb.failures != len(got) {
				t.Errorf("the breaker holds %d failures, want one per call (%d)", cb.failures, len(got))
			}
		})
	}
}

// ChatJSONWithRetry's own guard, whatever the provider says: a caller that already asked to think least is
// not asked again after an empty JSON refusal, so the refusal is returned after one call.
func TestChatJSONWithRetry_ACallerThatAskedToThinkLeastIsNotAskedAgain(t *testing.T) {
	f := &fakeLLM{outs: []string{"", `{"ok":true}`}, errs: []error{errEmptyJSONAnswer, nil}}
	_, err := ChatJSONWithRetry(context.Background(), f, nil, "sys", "user", ChatOptions{LeastThinking: true}, validJSON)
	if !errors.Is(err, errEmptyJSONAnswer) || f.calls != 1 {
		t.Fatalf("calls=%d err=%v, want the refusal after one call", f.calls, err)
	}
}

// Only that refusal is retried. A json_validate_failed whose failed_generation holds what the model wrote is a
// model that answered badly, not one that ran out of room; tool_use_failed and any other 400 are the request's
// fault; and the same body with any status but 400 isn't the host's JSON check. Each is returned after one
// call, recorded in the breaker.
func TestChatJSONWithRetry_NoOther400IsRetried(t *testing.T) {
	refusals := map[string]struct {
		status int
		body   string
	}{
		"a non-empty failed_generation": {http.StatusBadRequest, `{"error":{"message":"Failed to generate JSON.","type":"invalid_request_error","code":"json_validate_failed","failed_generation":"{\"plan\":[{\"id\""}}`},
		"tool_use_failed":               {http.StatusBadRequest, `{"error":{"message":"Failed to call a function.","type":"invalid_request_error","code":"tool_use_failed","failed_generation":""}}`},
		"another 400":                   {http.StatusBadRequest, `{"error":{"message":"'messages' must contain the word 'json'","type":"invalid_request_error","code":"invalid_request_error"}}`},
		"a body that isn't JSON":        {http.StatusBadRequest, `Bad Request`},
		"that refusal with a 422":       {http.StatusUnprocessableEntity, groqRefusal},
	}
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			host := newGroqHost(t)
			host.mu.Lock()
			host.refusal, host.status = refusal.body, refusal.status
			host.mu.Unlock()
			p := NewOpenAICompatibleProviderWithOpts("k", "openai/gpt-oss-20b", "", host.URL+"/groq/openai/v1", 0, ProviderOpenAICompatible, ProviderOptions{})
			p.reasoning = true
			cb := NewCircuitBreaker(5, time.Minute)
			_, err := ChatJSONWithRetry(context.Background(), p, cb, "sys", "plan my week", ChatOptions{MaxTokens: 460}, validJSON)
			if err == nil || errors.Is(err, errEmptyJSONAnswer) {
				t.Fatalf("a 400 is an error, and not the empty-answer refusal: %v", err)
			}
			if got := host.sentEfforts(); len(got) != 1 {
				t.Errorf("asked %d times, want 1 (efforts %q)", len(got), got)
			}
			if cb.failures != 1 {
				t.Errorf("the breaker holds %d failures, want 1", cb.failures)
			}
		})
	}
}
