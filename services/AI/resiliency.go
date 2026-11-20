package ai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Sentinel errors for AI resiliency.
var (
	ErrCircuitOpen = errors.New("ai: circuit breaker is open — LLM provider may be down")
	ErrRateLimited = errors.New("ai: rate limit exceeded — please try again later")

	ErrAIDisabled = errors.New("ai: service is disabled")

	// ErrProviderRateLimited marks a provider-side rate limit (HTTP 429) that
	// survived retry/backoff. It means the provider is HEALTHY but throttling
	// us - so it must NOT be counted as a circuit-breaker failure (see
	// RecordResult). Providers wrap this so callers can detect it via
	// errors.Is.
	ErrProviderRateLimited = errors.New("ai: provider rate limit exceeded (429)")

	// ErrWorkspaceTokenBudgetExceeded / ErrUserTokenBudgetExceeded are returned
	// by the provider layer when the workspace or the per-user daily AI token
	// cap is already reached (see budget.go). Callers map them to a friendly
	// message via FriendlyProviderError.
	ErrWorkspaceTokenBudgetExceeded = errors.New("ai: workspace daily token budget exceeded")
	ErrUserTokenBudgetExceeded      = errors.New("ai: per-user daily token budget exceeded")

	// ErrAgentTokenBudgetExceeded / ErrChannelTokenBudgetExceeded are returned
	// when an extra budget dimension (a specific AI teammate, or a channel) has
	// reached its own configured daily cap. Mapped to friendly text the same way.
	ErrAgentTokenBudgetExceeded   = errors.New("ai: per-agent daily token budget exceeded")
	ErrChannelTokenBudgetExceeded = errors.New("ai: per-channel daily token budget exceeded")
)

// CircuitState represents the current state of the circuit breaker.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitOpen                         // Failing — reject requests
	CircuitHalfOpen                     // Testing — allow one request
)

// CircuitBreaker implements a simple circuit breaker pattern for LLM calls.
// Opens after maxFailures consecutive failures, half-opens after resetTimeout.
type CircuitBreaker struct {
	mu           sync.RWMutex
	state        CircuitState
	failures     int
	maxFailures  int
	lastFailure  time.Time
	resetTimeout time.Duration
}

// NewCircuitBreaker creates a circuit breaker with the given thresholds.
func NewCircuitBreaker(maxFailures int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:        CircuitClosed,
		maxFailures:  maxFailures,
		resetTimeout: resetTimeout,
	}
}

// Allow checks if a request should be allowed through the circuit breaker.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitOpen:
		// Check if enough time has passed to transition to half-open
		if time.Since(cb.lastFailure) > cb.resetTimeout {
			cb.state = CircuitHalfOpen
			helpers.MessageLogs.InfoLog.Printf("AI circuit breaker HALF-OPEN: allowing test request after %s", cb.resetTimeout)
			return nil
		}
		return ErrCircuitOpen
	default:
		return nil
	}
}

// RecordSuccess records a successful LLM call, resetting the circuit breaker.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.state = CircuitClosed
}

// RecordFailure records a failed LLM call, potentially opening the circuit.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()

	if cb.failures >= cb.maxFailures {
		cb.state = CircuitOpen
		helpers.MessageLogs.ErrorLog.Printf(
			"AI circuit breaker OPEN: %d consecutive failures. Will retry after %s",
			cb.failures, cb.resetTimeout)
	}
}

// RecordResult updates the breaker from a call outcome and is the preferred
// way to report results. It exists so a provider RATE LIMIT (HTTP 429) is
// treated as NEUTRAL rather than a failure: a 429 means the provider is up but
// throttling us, so counting it would let a busy/free-tier provider trip the
// breaker and block every user even though nothing is actually down. Behaviour:
//   - nil error            -> RecordSuccess (closes/keeps the circuit closed)
//   - provider rate limit  -> no-op (neither success nor failure)
//   - call called off      -> no-op: the caller stopped waiting (a panel
//     closed, a question asked again), which says nothing about the provider
//   - any other error      -> RecordFailure (counts toward opening)
//
// Counting the called-off ones opened the breaker on the demo on 11 Oct:
// five cancelled embeddings in one second, and the next person to ask was
// refused 503 for half a minute with nothing wrong.
func (cb *CircuitBreaker) RecordResult(err error) {
	switch {
	case err == nil:
		cb.RecordSuccess()
	case errors.Is(err, context.Canceled):
		// The caller went away; the provider may be fine. Neither outcome.
	case IsContextOverflow(err):
		// Refused for its size: the provider is up, and a smaller request goes through. Counted, ten board
		// questions on the demo would open the breaker and refuse every ask (V-157).
	case errors.Is(err, ErrProviderRateLimited):
		// Healthy provider, just throttling. Don't penalize the breaker.
	case errors.Is(err, ErrWorkspaceTokenBudgetExceeded), errors.Is(err, ErrUserTokenBudgetExceeded), errors.Is(err, ErrAgentTokenBudgetExceeded), errors.Is(err, ErrChannelTokenBudgetExceeded):
		// Budget block, not a provider failure. Don't penalize the breaker.
	default:
		cb.RecordFailure()
	}
}

// State returns the current circuit breaker state as a string.
func (cb *CircuitBreaker) State() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	switch cb.state {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// ResiliencyManager combines circuit breaking and rate limiting.
type ResiliencyManager struct {
	CB              *CircuitBreaker
	rateLimitPerMin int
}

// NewResiliencyManager creates a new resiliency manager.
func NewResiliencyManager(rateLimitPerMin int) *ResiliencyManager {
	return &ResiliencyManager{
		CB: NewCircuitBreaker(
			5,              // Open after 5 consecutive failures
			30*time.Second, // Try again after 30 seconds
		),
		rateLimitPerMin: rateLimitPerMin,
	}
}

// envInt reads a non-negative integer env var; returns 0 when unset/invalid.
func envInt(key string) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// allowAIRate is the limiter CheckRateLimit asks; a test swaps it to pin the window it returns.
var allowAIRate = redisStore.AllowFixedWindow

// CheckRateLimit verifies the user hasn't exceeded their per-minute AI request limit.
// Backed by registry.AIRate; the store helper handles the increment+TTL pin atomically.
func (rm *ResiliencyManager) CheckRateLimit(ctx context.Context, userUUID string) error {
	res := allowAIRate(ctx, registry.AIRate, []string{userUUID}, rm.rateLimitPerMin)
	if !res.Allowed {
		// The refusal already holds the window's end (the limiter read its TTL), so the answer can say when.
		return RateLimitedFor(res.RetryAfter)
	}
	return nil
}

// GetRateLimitRemaining returns how many AI requests the user has left this minute.
func (rm *ResiliencyManager) GetRateLimitRemaining(ctx context.Context, userUUID string) int {
	count, found, err := redisStore.GetInt64(ctx, registry.AIRate, []string{userUUID})
	if err != nil || !found {
		return rm.rateLimitPerMin
	}

	remaining := rm.rateLimitPerMin - int(count)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// PreCheck runs all resiliency checks before making an AI call.
// Returns nil if the request should proceed, or an error if it should be rejected.
func (rm *ResiliencyManager) PreCheck(ctx context.Context, userUUID string) error {
	// 1. Circuit breaker check
	if err := rm.CB.Allow(); err != nil {
		return err
	}

	// 2. Rate limit check
	if err := rm.CheckRateLimit(ctx, userUUID); err != nil {
		return err
	}

	return nil
}

// FriendlyProviderError turns a raw provider/transport error into a short,
// user-safe message for the chat UI. Raw errors leak internal hosts (e.g.
// "Post http://ollama:11434/api/chat: context deadline exceeded") and are
// meaningless to end users, so we map the common failure modes to actionable
// guidance and fall back to a generic line for anything else.
func FriendlyProviderError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())

	switch {
	// A refusal for size first, however it is known (by type, or by the words providers use): its numbers can hold
	// "429" ("Requested 10429"), which the substring cases below would read as a rate limit, and an identical retry
	// overflows identically, so "try again" is the one advice that is wrong for it (V-157). ClientError answers it 422.
	case IsContextOverflow(err):
		return overflowUserMessage
	case errors.Is(err, ErrOllamaUpdateRequired):
		return "This model needs a newer Ollama. Update Ollama on the AI server, then pull the model again."
	case errors.Is(err, errServiceDisabled) || strings.Contains(msg, "ai service is not enabled"):
		return AIOffWords
	case errors.Is(err, ErrCircuitOpen):
		return "The AI provider isn't answering right now. Please try again in a minute."
	case errors.Is(err, ErrUserTokenBudgetExceeded):
		return "You've reached your daily AI usage limit. It resets tomorrow, or an admin can raise your limit under Admin > AI & agents."
	case errors.Is(err, ErrAgentTokenBudgetExceeded):
		return "This AI teammate has reached its daily usage limit. It resets tomorrow, or an admin can raise the agent's limit under Admin > AI & agents."
	case errors.Is(err, ErrChannelTokenBudgetExceeded):
		return "This channel has reached its daily AI usage limit. It resets tomorrow, or an admin can raise the channel's limit under Admin > AI & agents."
	case errors.Is(err, ErrWorkspaceTokenBudgetExceeded):
		return "The workspace has reached its daily AI usage limit. It resets tomorrow, or an admin can raise the limit under Admin > AI & agents."
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out"):
		return "The AI model took too long to respond. If it's a local model it may still be loading - try again in a moment, or pick a faster model in the assistant's model menu."
	case errors.Is(err, context.Canceled) || strings.Contains(msg, "context canceled"):
		return "The request was cancelled."
	// The workspace's own per-person limit, before the provider's: no provider refused anything.
	case errors.Is(err, ErrRateLimited):
		return "You're asking the AI faster than this workspace allows. Please wait a minute and try again."
	case errors.Is(err, ErrProviderRateLimited) || strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "too many requests"):
		return "The AI provider is rate-limiting requests right now. Please wait a few seconds and try again."
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "dial ") || strings.Contains(msg, "connect: ") || strings.Contains(msg, "eof"):
		return "Can't reach the AI model server right now. If you're running a local model, make sure it's up; otherwise check the provider settings under Admin > AI & agents."
	case strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid api key") || strings.Contains(msg, "api key"):
		return "The AI provider rejected the request (authentication). An admin may need to update the API key under Admin > AI & agents."
	case strings.Contains(msg, "status 404") || strings.Contains(msg, "model not found") || strings.Contains(msg, "not found"):
		return "The selected AI model isn't available. An admin may need to pull or re-select it under Admin > AI & agents."
	default:
		return genericFailure
	}
}

// ClientError is the status and the words a person is answered with when an AI call fails (V-157). A request too
// large is a 422 the person can act on, never a 500.
//   - A failure of the AI itself (too large, rate or budget, breaker open, AI off, too slow) gets
//     FriendlyProviderError's words.
//   - So does any error that holds text written outside the workspace (foreignText): a provider's reply, a remote
//     agent's message, a failed connection. That text can name the provider's account (Groq's 413 carried the
//     organization id), its hosts and its limits, so it belongs in the server's log only.
//   - Any other error is one no case names: it may hold a store's or a library's own text, so the person gets the
//     generic line and the text goes to the log (UX-IA-H3 M2). A caller that keeps its own status (a 400 on the
//     person's input) uses ClientWords, which keeps words the workspace wrote for people ("that image is too large to
//     analyze (limit 8 MB)").
//
// The text it withholds goes to the server's log with ctx's trace, so no caller can drop it.
func ClientError(ctx context.Context, err error) (int, string) {
	status, words, named := clientAnswer(ctx, err)
	if !named {
		helpers.LogWarnWithContext(ctx, "ai: an error no case names was answered in the generic words; its text withheld: %+v", err)
		return status, genericFailure
	}
	return status, words
}

// clientAnswer is ClientError's status and words; named is false when no case names err and the words are its own.
func clientAnswer(ctx context.Context, err error) (int, string, bool) {
	if err == nil {
		return http.StatusInternalServerError, FriendlyProviderError(err), true
	}
	var pw *peopleWords
	if errors.As(err, &pw) {
		return pw.status, pw.words, true
	}
	foreign := foreignText(err)
	if foreign {
		helpers.LogWarnWithContext(ctx, "ai: a person was answered in our words; the provider's or connection's text withheld: %+v", err)
	}
	status := http.StatusInternalServerError
	switch {
	case IsContextOverflow(err):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrProviderRateLimited), errors.Is(err, ErrUserTokenBudgetExceeded),
		errors.Is(err, ErrAgentTokenBudgetExceeded), errors.Is(err, ErrChannelTokenBudgetExceeded), errors.Is(err, ErrWorkspaceTokenBudgetExceeded):
		status = http.StatusTooManyRequests
	case errors.Is(err, ErrCircuitOpen), errors.Is(err, errServiceDisabled), strings.Contains(strings.ToLower(err.Error()), "ai service is not enabled"):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, ErrOllamaUpdateRequired), foreign:
	default:
		return status, err.Error(), false
	}
	return status, FriendlyProviderError(err), true
}

// foreignText reports whether err holds text written outside the workspace: a provider's or a remote agent's reply
// (ProviderError), or a failed connection, whose text names hosts and addresses. A *url.Error from url.Parse is a
// setting's own check and made no connection.
func foreignText(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Op != "parse" {
		return true
	}
	var oe *net.OpError
	var de *net.DNSError
	if errors.As(err, &oe) || errors.As(err, &de) {
		return true
	}
	// A connection's text keeps its mark when a %v on the way dropped its type.
	msg := err.Error()
	return strings.Contains(msg, "dial tcp") || strings.Contains(msg, "dial udp")
}

// peopleWords is an error whose words were written for the person who asked, such as a refusal of an action they
// may not take: ClientError and ClientWords answer them as they are, with status (CR-437 N1).
type peopleWords struct {
	status int
	words  string
}

func (e *peopleWords) Error() string { return e.words }

// ForPeople is words written for the person who asked, such as a refusal, with the status they're answered with.
func ForPeople(status int, words string) error { return &peopleWords{status: status, words: words} }

// machineMarks finds text a library, a store or a connection wrote rather than people. It names internal hosts,
// files, queries and values, so ClientWords answers it in the generic line (CR-437 N2; FIX-2: every class CR-437's
// probe got back raw). One alternation per class.
var machineMarks = regexp.MustCompile(strings.Join([]string{
	// URL: a request line (`Post "http://ollama:11434/api/chat": EOF`) or any scheme's URL with a host.
	`\b(Get|Head|Post|Put|Patch|Delete|Options) "|(?i:\b[a-z][a-z0-9+.-]*://[\w\[])`,
	// Address: an IPv4, a host:port in any case (`OLLAMA:11434`), an IPv6 with a port (`[::1]:11434`), a DSN's keys.
	`\b\d{1,3}(\.\d{1,3}){3}\b|(?i:\b[a-z][a-z0-9.-]*:\d{2,5}\b)|\[[0-9a-fA-F:]+\]:\d+|\b(host|port|user|dbname|sslmode|password)=`,
	// Path: two or more segments, absolute or relative (`/var/lib/...`, `./templates/...`).
	`(^|[\s"'(=:])\.{0,2}/[\w.-]+/`,
	// Connection: its own words, also when a %v on the way flattened a deadline.
	`\bEOF\b|i/o timeout|connection (refused|reset)|no such host|broken pipe|\btls: |\bx509: |context deadline exceeded`,
	// SQL: a driver's prefix, Postgres's `ERROR: ` and constraint words, a raw query.
	`\bpq: |SQLSTATE|\bsql: |\bERROR: |violates [\w-]+( key)? constraint|\b(SELECT|INSERT|UPDATE|DELETE)\b[^.]*\b(FROM|INTO|SET|WHERE)\b`,
	// Store: gRPC, Redis (its name, `ERR ` or a reply's code), dgo's abort, OpenSearch's status and JSON body, minio.
	`rpc error|(?i:\bredis\b|\bdgraph\b)|\bERR |(^|: )(WRONGTYPE|NOAUTH|NOPERM|MOVED|LOADING|READONLY|NOSCRIPT|EXECABORT|CROSSSLOT|MISCONF) |` +
		`Transaction has been aborted|\[\d{3} [A-Za-z ]+\] \{|\bminio: `,
	// JSON: encoding/json's prefix and its decoder's words.
	`\bjson: |Go (struct field|value of type)|looking for beginning of value|unexpected end of JSON input`,
	// Go value: a type, a pointer or a value printed by Go (`*ai.ProviderError`, `&{...}`, `0xc000123400`, `map[...]`).
	`\b0x[0-9a-f]{6,}\b|\*[a-z]\w*\.[A-Za-z]\w*|&\{|\b[a-z]\w*\.[A-Z]\w*\{|interface \{\}|map\[\w+\]`,
	// Stack: a goroutine, a panic, a runtime error, a frame (`pkg.(*T).Method(0x...)`, `file.go:7`), a line break.
	`goroutine |\bpanic: |\bruntime error: |\.\(\*?\w+\)\.\w+\(|\.go:\d+\b|\n`,
}, "|"))

// typedURL is a url.Parse error's quoted URL (`parse "http://exa mple.com": invalid character " " in host name`):
// a setting a person typed, so ClientWords drops its scheme before the URL mark reads it; every other mark still does.
var typedURL = regexp.MustCompile(`\bparse "[a-zA-Z][a-zA-Z0-9+.-]*://`)

// ClientWords is ClientError's words for a caller that keeps its own status: words written for people stay theirs.
// A text no case names keeps its words unless a library, a store or a connection wrote it (machineMarks): that
// text goes to the log and the person gets the generic line.
func ClientWords(ctx context.Context, err error) string {
	_, words, named := clientAnswer(ctx, err)
	if !named && machineMarks.MatchString(typedURL.ReplaceAllString(words, `parse "`)) {
		helpers.LogWarnWithContext(ctx, "ai: a library's or connection's text was answered in the generic words; its text withheld: %+v", err)
		return genericFailure
	}
	return words
}

// AIOffWords is what every AI route answers while AI is off in the workspace: what happened and who can change it.
const AIOffWords = "AI is turned off for this workspace. An admin can turn it on under Admin > AI & agents."

// genericFailure is the line for an AI failure no case names.
const genericFailure = "The AI model couldn't complete that request. Please try again."

// rateWindow is a refusal that knows when it ends: the limiter's own window.
type rateWindow struct{ after time.Duration }

func (e *rateWindow) Error() string { return ErrRateLimited.Error() }
func (e *rateWindow) Unwrap() error { return ErrRateLimited }

// RateLimitedFor is ErrRateLimited carrying how long until the limiter's window ends; 0 when it isn't known.
func RateLimitedFor(after time.Duration) error { return &rateWindow{after: after} }

// retryNow is the clock RetryAfterSeconds reads; a test sets it.
var retryNow = time.Now

// RetryAfterSeconds is the Retry-After an AI answer with these words carries for err, in whole seconds; 0 is none.
// The web offers Try again exactly when an answer carries the header, so it follows the words: an answer that tells
// the person to try again carries it, and no other does (a daily limit "resets tomorrow"; AI off needs an admin).
// The wait is the breaker's minute, the limiter's own window (else to the next minute), or 5 seconds.
func RetryAfterSeconds(err error, words string) int {
	if !strings.Contains(strings.ToLower(words), "try again") {
		return 0
	}
	var w *rateWindow
	switch {
	case errors.Is(err, ErrCircuitOpen):
		return 60
	case errors.As(err, &w) && w.after > 0:
		return wholeSeconds(w.after)
	case errors.Is(err, ErrRateLimited):
		now := retryNow()
		return wholeSeconds(now.Truncate(time.Minute).Add(time.Minute).Sub(now))
	default:
		return 5
	}
}

// wholeSeconds rounds d up, so a client never asks again before the wait is over.
func wholeSeconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}

// SetRetryAfter sets an AI answer's Retry-After on h, by RetryAfterSeconds. Call it before the status is written.
// A stream's error after its first byte can't carry a header: the SSE frame has the words only.
func SetRetryAfter(h http.Header, err error, words string) {
	if s := RetryAfterSeconds(err, words); s > 0 {
		h.Set("Retry-After", strconv.Itoa(s))
	}
}
