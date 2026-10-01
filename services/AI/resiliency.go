package ai

import (
	"context"
	"errors"
	"os"
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
//   - any other error      -> RecordFailure (counts toward opening)
func (cb *CircuitBreaker) RecordResult(err error) {
	switch {
	case err == nil:
		cb.RecordSuccess()
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

// CheckRateLimit verifies the user hasn't exceeded their per-minute AI request limit.
// Backed by registry.AIRate; the store helper handles the increment+TTL pin atomically.
func (rm *ResiliencyManager) CheckRateLimit(ctx context.Context, userUUID string) error {
	res := redisStore.AllowFixedWindow(ctx, registry.AIRate, []string{userUUID}, rm.rateLimitPerMin)
	if !res.Allowed {
		return ErrRateLimited
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
	case errors.Is(err, ErrUserTokenBudgetExceeded):
		return "You've reached your daily AI usage limit. It resets tomorrow, or an admin can raise your limit under Admin > AI Models."
	case errors.Is(err, ErrAgentTokenBudgetExceeded):
		return "This AI teammate has reached its daily usage limit. It resets tomorrow, or an admin can raise the agent's limit under Admin > AI Models."
	case errors.Is(err, ErrChannelTokenBudgetExceeded):
		return "This channel has reached its daily AI usage limit. It resets tomorrow, or an admin can raise the channel's limit under Admin > AI Models."
	case errors.Is(err, ErrWorkspaceTokenBudgetExceeded):
		return "The workspace has reached its daily AI usage limit. It resets tomorrow, or an admin can raise the limit under Admin > AI Models."
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out"):
		return "The AI model took too long to respond. If it's a local model it may still be loading - try again in a moment, or pick a faster model in the assistant's model menu."
	case errors.Is(err, context.Canceled) || strings.Contains(msg, "context canceled"):
		return "The request was cancelled."
	case errors.Is(err, ErrProviderRateLimited) || strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "too many requests"):
		return "The AI provider is rate-limiting requests right now. Please wait a few seconds and try again."
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "dial ") || strings.Contains(msg, "connect: ") || strings.Contains(msg, "eof"):
		return "Can't reach the AI model server right now. If you're running a local model, make sure it's up; otherwise check the provider settings under Admin > AI Models."
	case strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid api key") || strings.Contains(msg, "api key"):
		return "The AI provider rejected the request (authentication). An admin may need to update the API key under Admin > AI Models."
	case strings.Contains(msg, "status 404") || strings.Contains(msg, "model not found") || strings.Contains(msg, "not found"):
		return "The selected AI model isn't available. An admin may need to pull or re-select it under Admin > AI Models."
	// Context overflow is checked LAST among the specific cases but before the
	// default, because it is the one failure where the default advice is actively
	// wrong: an identical retry overflows identically. Providers phrase it a dozen
	// ways with no portable status code, so IsContextOverflow owns the matching.
	case describeOverflow(err) != "":
		return describeOverflow(err)
	default:
		return "The AI model couldn't complete that request. Please try again."
	}
}
