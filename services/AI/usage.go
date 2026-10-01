package ai

// Per-call token accounting. The LLMProvider.Chat signature returns only text,
// so to surface real token usage to callers (the agent runner's per-run budget,
// cost reporting) without a breaking interface change, providers report usage
// into a context-scoped sink. A caller attaches the sink once with
// WithUsageSink, then reads each model call's usage with TakeUsage. When no
// sink is attached (the default, e.g. the interactive assistant), reportUsage
// is a no-op, so behavior is unchanged for non-metered callers.

import (
	"context"
	"sync"
)

// Usage is the token usage of a single completion.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total is the sum of input + output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

type usageSink struct {
	mu sync.Mutex
	u  Usage
}

type usageCtxKeyT struct{}

var usageCtxKey usageCtxKeyT

// WithUsageSink returns a context that accumulates token usage reported by the
// provider layer. Idempotent: if a sink is already attached, the same context
// is returned so nested calls share one accumulator.
func WithUsageSink(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(usageCtxKey) != nil {
		return ctx
	}
	return context.WithValue(ctx, usageCtxKey, &usageSink{})
}

// reportUsage folds a provider's reported usage into the context sink, if one
// is attached. No-op otherwise. Called by providers after a completion.
func reportUsage(ctx context.Context, u Usage) {
	if ctx == nil {
		return
	}
	s, _ := ctx.Value(usageCtxKey).(*usageSink)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.u.InputTokens += u.InputTokens
	s.u.OutputTokens += u.OutputTokens
	s.mu.Unlock()
}

// TakeUsage returns the usage accumulated since the last call and resets the
// sink to zero, so a caller can attribute usage per model call. Returns the
// zero Usage when no sink is attached.
func TakeUsage(ctx context.Context) Usage {
	if ctx == nil {
		return Usage{}
	}
	s, _ := ctx.Value(usageCtxKey).(*usageSink)
	if s == nil {
		return Usage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.u
	s.u = Usage{}
	return u
}
