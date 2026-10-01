package ai

// Per-model token limits.
//
// OneCamp is model-agnostic: an admin authorizes a set of models, and a member, a
// channel or an agent then picks one from that set. So "the token limit" is not one
// number — there is one per model, and which one applies depends on who is asking.
//
// Before this file there was exactly one: ai_settings.context_window_tokens, read
// from the global config singleton by every budget calculation in the codebase. That
// is correct only when every model shares a window, which is the one thing a
// model-agnostic product cannot assume. Pinning a channel to a 128k model while the
// workspace said 8192 discarded context that would have fitted AND ran the model at
// num_ctx=8192; pinning an 8k model while the workspace said 128k built prompts the
// model could not accept.
//
// ModelLimits makes the limit a VALUE that travels with the model, so a budget is
// computed from the model that will actually answer. The package-level helpers in
// contextBudget.go still exist and still mean "the workspace default", which is the
// right answer for the many callers that genuinely have no model in hand — they are
// now thin wrappers over WorkspaceLimits(), so there is one implementation rather
// than two that can drift.

import (
	"context"
	"strings"
)

// ModelLimits is everything a token budget needs to know about one model.
//
// Carries Provider and Model, not just the numbers, because the tokenizer ratio is
// also per-model and is looked up by that pair. Bundling them is what stops a
// caller from correctly using a pinned model's window while estimating its text
// with the default model's tokenizer — which is exactly the bug this replaced.
type ModelLimits struct {
	// Provider and Model identify the model these limits describe. Used for the
	// calibrated chars-per-token lookup; empty means "unidentified", which falls
	// back to the static heuristic.
	Provider string
	Model    string

	// ContextWindow is the total tokens the model accepts (prompt + completion).
	// Always > 0 and never below minContextWindow.
	ContextWindow int

	// MaxOutput is the most tokens this model will generate in one response, or 0 for
	// "not stated, use the caller's own reservation".
	//
	// Separate from ContextWindow because the two are frequently far apart: a model
	// may read a million tokens and still refuse to write more than a few tens of
	// thousands. A budget that assumed output was bounded only by the window would
	// reserve far too much of it.
	MaxOutput int

	// Source records where ContextWindow came from ("model", "workspace", "default"),
	// for admin-facing diagnostics. Cheap to carry and the first thing worth knowing
	// when a budget looks wrong.
	Source string
}

const (
	// limitSourceModel: the admin stated this model's window on its allowlist row.
	limitSourceModel = "model"
	// limitSourceWorkspace: from ai_settings.context_window_tokens.
	limitSourceWorkspace = "workspace"
	// limitSourceDefault: no config at all; the built-in fallback.
	limitSourceDefault = "default"
)

// limitsForEndpoint builds limits for an endpoint from an EXPLICIT config, for callers
// that already hold one. Same result as LimitsForModel with no stated values, without the
// hidden read of the global config singleton.
func limitsForEndpoint(ep Endpoint, cfg *AIConfig) ModelLimits {
	window := defaultContextWindow
	source := limitSourceDefault
	if cfg != nil {
		window, source = cfg.EffectiveContextWindow(), limitSourceWorkspace
	}
	if window < minContextWindow {
		window = minContextWindow
	}
	return ModelLimits{
		Provider:      string(ep.Kind),
		Model:         ep.Model,
		ContextWindow: window,
		Source:        source,
	}
}

// WorkspaceLimits returns the limits of the workspace DEFAULT model — today's
// behaviour for every caller that has no particular model in hand.
func WorkspaceLimits() ModelLimits {
	lim := ModelLimits{ContextWindow: contextWindowTokens(), Source: limitSourceWorkspace}
	if cfg := GetConfig(); cfg != nil {
		lim.Provider = string(cfg.Provider())
		lim.Model = cfg.ActiveModel()
		if cfg.ContextWindowTokens <= 0 && cfg.OllamaNumCtx <= 0 {
			lim.Source = limitSourceDefault
		}
	} else {
		lim.Source = limitSourceDefault
	}
	return lim
}

// LimitsForModel builds the limits to use when a SPECIFIC model answers.
//
// rowWindow / rowOutput are what the admin recorded on this model's allowlist row
// (migration 140); 0 for either means "not stated" and inherits the workspace value.
//
// Deliberately does not guess a window from the model's NAME when the admin left it
// blank. A compiled-in table of "gpt-4o is 128k" is wrong the week a provider ships a
// new model, cannot cover the openai_compatible kind (any endpoint, including a router
// serving different models under one name) or an arbitrary locally-pulled Ollama tag,
// and would CHANGE the effective window on upgrade for a workspace that had chosen a
// small one deliberately to fit its VRAM. What an operator wrote down wins; absent
// that, nothing here claims to know better than the workspace setting they already made.
func LimitsForModel(provider, model string, rowWindow, rowOutput int) ModelLimits {
	lim := ModelLimits{
		Provider:  strings.TrimSpace(provider),
		Model:     strings.TrimSpace(model),
		MaxOutput: rowOutput,
	}
	if rowWindow > 0 {
		lim.ContextWindow, lim.Source = rowWindow, limitSourceModel
	} else {
		ws := WorkspaceLimits()
		lim.ContextWindow, lim.Source = ws.ContextWindow, ws.Source
	}
	if lim.ContextWindow < minContextWindow {
		lim.ContextWindow = minContextWindow
	}
	if lim.MaxOutput < 0 {
		lim.MaxOutput = 0
	}
	return lim
}

// --- tokenizer ratio (per model) ---------------------------------------------

// CharsPerToken returns the calibrated chars-per-token ratio for THIS model,
// falling back to the static heuristic until the model has enough samples.
//
// The calibrator has always stored ratios per (provider, model) — correctly — but
// every read went through the workspace's ACTIVE model, so a channel pinned to a
// different model was measured with the default model's tokenizer. Providers differ
// by 15-30% on the same text, and a single tokenizer change within one vendor's line
// has moved it by that much, so the error was large enough to overflow a window the
// budget believed it was respecting.
func (l ModelLimits) CharsPerToken() float64 {
	if r, ok := globalCalibrator.ratio(l.Provider, l.Model); ok {
		return r
	}
	return float64(avgCharsPerToken)
}

// EstimateTokens is a conservative (rounded-up) token estimate for s under THIS
// model's tokenizer. Rune-based so multibyte text is not under-counted.
func (l ModelLimits) EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return int(float64(len([]rune(s)))/l.CharsPerToken() + 0.9999)
}

// --- budgets ------------------------------------------------------------------

// ResponseReserve is the tokens to hold back for the model's output, given what the
// caller asked for.
//
// A stated ceiling wins only when it is SMALLER. Asking a model for more than it will
// produce does not get more output; it reserves window that could have held context. A
// larger stated ceiling is never imposed on a caller that asked for less — the caller
// knows what it needs, and the provider clients already handle the per-request field
// correctly and differently from each other (OpenAI sends max_completion_tokens so
// reasoning models accept it; Anthropic's API requires it and has a documented default).
func (l ModelLimits) ResponseReserve(want int) int {
	if want <= 0 {
		want = responseTokenReserve
	}
	if l.MaxOutput > 0 && l.MaxOutput < want {
		return l.MaxOutput
	}
	return want
}

// AvailableInputTokens is the budget left for history + workspace context after
// reserving room for the response and the prompt scaffold.
func (l ModelLimits) AvailableInputTokens() int {
	avail := l.ContextWindow - l.ResponseReserve(0) - scaffoldTokenReserve
	if avail < 256 {
		// Pathologically small window: keep a minimal working budget rather than
		// returning zero, which would drop all context and history.
		avail = 256
	}
	return avail
}

// HistoryBudget is the maximum tokens allotted to session history.
func (l ModelLimits) HistoryBudget() int {
	return l.AvailableInputTokens() * historyBudgetNumerator / historyBudgetDenominator
}

// ContextBudget is the maximum tokens allotted to the assembled workspace-context
// block.
func (l ModelLimits) ContextBudget() int {
	return l.AvailableInputTokens() - l.HistoryBudget()
}

// DocInputBudget is the max input tokens for a one-shot doc action that reserves
// outputTokens for its answer. No history, so the only other cost is the fixed
// action system-prompt.
func (l ModelLimits) DocInputBudget(outputTokens int) int {
	avail := l.ContextWindow - l.ResponseReserve(outputTokens) - scaffoldTokenReserve
	if avail < 256 {
		avail = 256
	}
	return avail
}

// TrimHistoryToBudget returns the most RECENT suffix of history fitting maxTokens,
// measured with this model's tokenizer.
func (l ModelLimits) TrimHistoryToBudget(history []ChatMessage, maxTokens int) []ChatMessage {
	if len(history) == 0 || maxTokens <= 0 {
		return nil
	}
	total := 0
	start := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		t := l.EstimateTokens(history[i].Content) + 4 // role/framing overhead
		if total+t > maxTokens {
			break
		}
		total += t
		start = i
	}
	if start == len(history) {
		return nil
	}
	return history[start:]
}

// TruncateToTokenBudget caps s to at most maxTokens (rune-safe), appending a visible
// marker so the model knows context was cut. Keeps the PREFIX, because context is
// assembled highest-priority-first and the tail is the supplementary material.
//
// Measures AND cuts with the same ratio. The previous implementation measured with
// the calibrated ratio but converted the surviving token budget to runes with the
// static 4, so for any model whose real ratio was below 4 — dense tokenizers, code,
// CJK — it kept more runes than the budget allowed and quietly overflowed the limit
// it existed to enforce. Two numbers for one conversion is one too many.
func (l ModelLimits) TruncateToTokenBudget(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if l.EstimateTokens(s) <= maxTokens {
		return s
	}
	const marker = "\n\n[Context truncated to fit the model's window.]"
	keepTokens := maxTokens - l.EstimateTokens(marker)
	if keepTokens < 1 {
		keepTokens = 1
	}
	keepRunes := int(float64(keepTokens) * l.CharsPerToken())
	runes := []rune(s)
	if keepRunes >= len(runes) {
		return s
	}
	// Prefer a newline boundary near the limit so a section header is not sliced in
	// half when avoiding it is free.
	cut := keepRunes
	if nl := strings.LastIndex(string(runes[:cut]), "\n"); nl > keepRunes*3/4 {
		cut = len([]rune(string(runes[:cut])[:nl]))
	}
	return strings.TrimRight(string(runes[:cut]), " \n") + marker
}

// --- carrying limits through a call ------------------------------------------

type modelLimitsKey struct{}

// WithModelLimits attaches the limits of the model that will serve this call, so
// budget code downstream measures against the right model without every function
// growing a parameter. Follows the same convention as WithActor / WithAgentBudget,
// which already carry per-call AI dimensions this way.
func WithModelLimits(ctx context.Context, lim ModelLimits) context.Context {
	if ctx == nil || lim.ContextWindow <= 0 {
		return ctx
	}
	return context.WithValue(ctx, modelLimitsKey{}, lim)
}

// LimitsFrom returns the limits attached to ctx, or the workspace default when none
// is. Never returns a zero value, so a caller can always budget with what it gets —
// a missing attachment degrades to today's behaviour rather than to a window of 0.
func LimitsFrom(ctx context.Context) ModelLimits {
	if ctx != nil {
		if lim, ok := ctx.Value(modelLimitsKey{}).(ModelLimits); ok && lim.ContextWindow > 0 {
			return lim
		}
	}
	return WorkspaceLimits()
}
