package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// The limiter's refusal carries its own window into the header: the TTL AllowFixedWindow read, not "the next
// minute" (CR-437 F3, mutant M6).
func TestTheLimitersWindowReachesTheHeader(t *testing.T) {
	savedNow, savedAllow := retryNow, allowAIRate
	t.Cleanup(func() { retryNow, allowAIRate = savedNow, savedAllow })
	// 17 s past the minute: "the next minute" would be 43, the limiter's window is 42.
	retryNow = func() time.Time { return time.Date(2026, 11, 18, 14, 5, 17, 0, time.UTC) }
	allowAIRate = func(context.Context, registry.Spec, []string, int) redisStore.RateLimitResult {
		return redisStore.RateLimitResult{Allowed: false, Count: 21, Limit: 20, RetryAfter: 41*time.Second + 300*time.Millisecond}
	}
	rm := NewResiliencyManager(20)
	err := rm.CheckRateLimit(context.Background(), "11111111-1111-4111-8111-111111111111")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("CheckRateLimit over the limit = %v, want ErrRateLimited", err)
	}
	if got := RetryAfterSeconds(err, "You've asked AI a lot in the last minute. Try again in a moment."); got != 42 {
		t.Errorf("Retry-After = %d, want the limiter's window rounded up, 42", got)
	}
	if err := rm.PreCheck(context.Background(), "u"); RetryAfterSeconds(err, "Try again.") != 42 {
		t.Errorf("PreCheck lost the limiter's window: %v", err)
	}
	allowAIRate = func(context.Context, registry.Spec, []string, int) redisStore.RateLimitResult {
		return redisStore.RateLimitResult{Allowed: true, Count: 1, Limit: 20}
	}
	if err := rm.CheckRateLimit(context.Background(), "u"); err != nil {
		t.Errorf("CheckRateLimit under the limit = %v, want nil", err)
	}
}

// A library's or a connection's text never reaches a person through ClientWords either: it names internal hosts
// (CR-437 N2: the probe got `Post "http://ollama:11434/api/chat": EOF` back raw). The person gets the generic line.
func TestClientWordsAnswersALibrarysTextInTheGenericLine(t *testing.T) {
	post := errors.New(`Post "http://ollama:11434/api/chat": EOF`)
	for _, err := range []error{
		post,
		fmt.Errorf("list models: %w", post),
		fmt.Errorf("list models: %v", post),
		errors.New(`pq: password authentication failed for user "onecamp" on host db.internal:5432`),
		errors.New("rpc error: code = Unavailable desc = connection error"),
		errors.New("redis: connection pool timeout"),
		errors.New("read tcp 172.18.0.4:51234->172.18.0.9:6379: i/o timeout"),
		errors.New("panic: runtime error\ngoroutine 1 [running]:"),
		errors.New("unexpected EOF"),
	} {
		got := ClientWords(context.Background(), err)
		if got != genericFailure {
			t.Errorf("ClientWords(%q) = %q, want the generic line", err, got)
		}
		for _, leak := range []string{"ollama", "11434", "pq:", "5432", "rpc error", "redis", "172.18", "goroutine", "EOF"} {
			if strings.Contains(got, leak) {
				t.Errorf("ClientWords(%q) leaked %q", err, leak)
			}
		}
	}

	// FIX-2 (MAIN 15:5x on CR-437's FIX-1 probe): every text the probe got back raw, word for word, one class per
	// group. Each gets the generic line, bare and wrapped with %w.
	for _, c := range []struct{ class, text string }{
		{"url", "unexpected status 502 from https://api.groq.com/openai/v1/chat/completions"},
		{"url", "GET http://ollama/api/tags failed with 500"},
		{"url", "fetch http://ollama.internal/api/tags: bad gateway"},
		{"url", "webhook https://hooks.example.com/abc returned 500"},
		{"address", "upstream [::1]:11434 closed"},
		{"address", "OLLAMA:11434 is down"},
		{"address", "host=db.internal port=5432 refused"},
		{"path", "open /var/lib/onecamp/uploads/abc.pdf: no such file or directory"},
		{"path", "stat /data/models/qwen.gguf: permission denied"},
		{"path", "read /tmp/onecamp-123/x: is a directory"},
		{"path", "load template ./templates/plan.tmpl failed"},
		{"sql", `ERROR: syntax error at or near "FROM"`},
		{"sql", "query SELECT id FROM users WHERE uuid = $1 failed"},
		{"sql", `duplicate key value violates unique constraint "ai_memories_pkey"`},
		{"store", "Transaction has been aborted. Please retry"},
		{"store", "WRONGTYPE Operation against a key..."},
		{"store", "NOAUTH Authentication required."},
		{"store", `[404 Not Found] {"error":{"type":"index_not_found_exception",...}}`},
		{"store", "minio: The specified key does not exist."},
		{"json", "json: cannot unmarshal number into Go struct field Memory.score of type string"},
		{"json", "invalid character '<' looking for beginning of value"},
		{"json", "unexpected end of JSON input"},
		{"connection", "context deadline exceeded"},
		{"go value", "unexpected type *ai.ProviderError"},
		{"go value", "unexpected type *errors.errorString"},
		{"go value", "got &{ID:5 Name:x Secret:abc}"},
		{"go value", "bad value 0xc000123400"},
		{"go value", "interface conversion: interface {} is nil, not map[string]interface {}"},
		{"go value", `models.User{UUID:"x", Email:"a@b.c"}`},
		{"go value", "cannot use map[string]interface {} as []string"},
		{"stack", "github.com/akashc777/OneCamp/business/AI.(*Svc).Ask(0xc000123400)"},
		{"stack", "failed at file.go:7"},
		{"stack", "runtime error: index out of range [3] with length 2"},
		{"stack", "runtime error: invalid memory address or nil pointer dereference"},
	} {
		for _, err := range []error{errors.New(c.text), fmt.Errorf("ask: %w", errors.New(c.text))} {
			if got := ClientWords(context.Background(), err); got != genericFailure {
				t.Errorf("%s: ClientWords(%q) = %q, want the generic line", c.class, err, got)
			}
		}
	}

	// No false positives: the six lines CR-437 wrote for people keep their words, bare and wrapped.
	for _, words := range []string{
		"that image is too large to analyze (limit 8 MB)",
		"there's no text to translate",
		"the model qwen3:4b-instruct isn't installed on this server",
		"pick a time between 9:00 and 17:00",
		"the instruction is longer than 2000 characters",
		"you're not a member of this project",
	} {
		for _, err := range []error{errors.New(words), fmt.Errorf("ask: %w", errors.New(words))} {
			if got := ClientWords(context.Background(), err); got != err.Error() {
				t.Errorf("ClientWords(%q) = %q: the words written for people were replaced", err, got)
			}
		}
	}
}

// A refusal written for the person (an action they may not take) keeps its words and its status through
// ClientError, wrapped or not (CR-437 N1: ExecuteAction's "you are not a member of this channel").
func TestARefusalWrittenForThePersonReachesThem(t *testing.T) {
	refusal := ForPeople(http.StatusForbidden, "you are not a member of this channel")
	for _, err := range []error{refusal, fmt.Errorf("execute send_message: %w", refusal)} {
		status, words := ClientError(context.Background(), err)
		if status != http.StatusForbidden || words != "you are not a member of this channel" {
			t.Errorf("ClientError(%q) = %d %q, want 403 and the refusal's words", err, status, words)
		}
		if got := ClientWords(context.Background(), err); got != "you are not a member of this channel" {
			t.Errorf("ClientWords(%q) = %q", err, got)
		}
		if RetryAfterSeconds(err, words) != 0 {
			t.Errorf("a refusal carries no Retry-After: %q", words)
		}
	}
}
