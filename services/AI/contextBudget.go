package ai

// Prompt token-budget management.
//
// WHY THIS EXISTS
// ---------------
// The AskAI prompt is assembled from many independently-capped sources
// (workspace structure, chronological history, structured memory, GraphRAG
// owned/scope items, semantic search results) plus the multi-turn session
// history. Each source has its own size cap, but nothing bounded the TOTAL.
//
// Local models served by Ollama run with a fixed context window (num_ctx,
// default 8192 tokens). When the assembled prompt exceeds that window the
// runtime silently truncates from the FRONT — dropping the system prompt
// and tool instructions first — which quietly degrades answer quality and
// tool-call reliability with no error surfaced. This module enforces a hard
// token budget so that never happens: history is trimmed by tokens (not a
// raw message count) and the workspace-context block is capped, both derived
// from the model's real context window.
//
// Token estimation is a deliberately simple, provider-agnostic heuristic
// (~4 chars/token) — we don't ship a tokenizer per model. It is tuned to
// OVER-estimate slightly (round up) so the budget stays conservative and we
// never accidentally overflow the window.

import (
	"sync"
)

const (
	// avgCharsPerToken is the heuristic used to estimate token counts from
	// character length. ~4 is the widely-used rule of thumb for English +
	// code across GPT/Llama-family tokenizers. We round UP when estimating
	// so the budget errs on the side of fitting.
	avgCharsPerToken = 4

	// defaultContextWindow mirrors the Ollama provider's num_ctx default. A
	// floor is applied so a misconfigured tiny window can't starve the
	// prompt to nothing.
	defaultContextWindow = 8192
	minContextWindow     = 2048

	// responseTokenReserve is held back for the model's OUTPUT (mirrors the
	// 1024 MaxTokens used by AskAI) so generation isn't starved.
	responseTokenReserve = 1024

	// scaffoldTokenReserve covers the system prompt, optional tool prompt,
	// the question itself, and message framing overhead — everything in the
	// prompt that is NOT history or the workspace-context block.
	scaffoldTokenReserve = 1024

	// historyBudgetFraction is the share of the available input budget given
	// to multi-turn session history. Context (the retrieved workspace
	// knowledge) is the priority for answer quality, so history gets the
	// smaller slice; the rest goes to the context block.
	historyBudgetNumerator   = 1
	historyBudgetDenominator = 4
)

// EstimateTokens returns a conservative (rounded-up) token estimate for s,
// using the active provider/model's CALIBRATED chars-per-token ratio when
// available, falling back to the ~4-chars/token heuristic. Rune-based so
// multibyte text isn't under-counted by byte length.
//
// This is the single estimation entry point used across budget code, so a
// more accurate ratio (learned from real provider usage reports) improves
// every call site at once.
// Estimates for the WORKSPACE DEFAULT model. Prefer
// ai.LimitsFrom(ctx).EstimateTokens(s) wherever a different model may answer —
// the ratio is per-model and differs enough between them to matter.
func EstimateTokens(s string) int {
	return WorkspaceLimits().EstimateTokens(s)
}

// contextWindowTokens returns the model's usable context window in tokens.
// Resolution order:
//  1. The live admin-managed config (ai_settings.context_window_tokens),
//     which also drives the provider's actual num_ctx — so the budget and
//     the model can never diverge.
//  2. An explicit AI_CONTEXT_TOKENS env override.
//  3. OLLAMA_NUM_CTX env (what the provider passes by default).
//  4. The 8192 default.
//
// Floored at minContextWindow so a misconfigured tiny value can't starve
// the prompt to nothing.
func contextWindowTokens() int {
	// Live config first: this is the single source that also sets num_ctx.
	if cfg := GetConfig(); cfg != nil {
		if w := cfg.EffectiveContextWindow(); w >= minContextWindow {
			return w
		}
	}
	if v := getEnvInt("AI_CONTEXT_TOKENS", 0); v > 0 {
		if v < minContextWindow {
			return minContextWindow
		}
		return v
	}
	v := getEnvInt("OLLAMA_NUM_CTX", defaultContextWindow)
	if v < minContextWindow {
		return minContextWindow
	}
	return v
}

// ContextTokenBudget is the maximum tokens allotted to the assembled
// workspace-context block (everything buildUserContext produces), for the
// WORKSPACE DEFAULT model. See HistoryTokenBudget on picking the model-aware form.
func ContextTokenBudget() int {
	return WorkspaceLimits().ContextBudget()
}

// TruncateToTokenBudget caps s to at most maxTokens (rune-safe), appending a
// visible marker when truncation occurs so the model knows context was cut.
// Truncation keeps the PREFIX — buildUserContext emits the highest-priority,
// deterministic context first (localization, workspace structure, recent
// history, structured memory) and the supplementary semantic-search results
// last, so dropping the tail degrades gracefully. maxTokens <= 0 returns "".
func TruncateToTokenBudget(s string, maxTokens int) string {
	return WorkspaceLimits().TruncateToTokenBudget(s, maxTokens)
}

// --- Provider-aware token calibration ---
//
// The static ~4-chars/token heuristic is fine for local Ollama models, but
// different providers/models tokenize differently (code-heavy, CJK, and
// some BPE vocabularies diverge 15-30%). Rather than ship a per-model
// tokenizer (heavy, and impossible for the model-agnostic
// openai_compatible path), we LEARN the real ratio from the usage that
// every cloud provider already reports: each Chat response carries the
// exact prompt-token count. We divide the characters we actually sent by
// that count and maintain a smoothed per-(provider,model) ratio.
//
// Properties:
//   - Self-correcting: an EWMA so it tracks the model without overreacting
//     to a single outlier request.
//   - Never stale: re-learns whenever the admin swaps models.
//   - Safe default: until a model has enough samples, EstimateTokens uses
//     the static heuristic, so behaviour is unchanged for local Ollama
//     (which we also calibrate when it reports prompt_eval_count).
//   - Bounded: the ratio is clamped to a sane range so a malformed usage
//     report can't make the estimator wildly over- or under-count.

const (
	// calibrationAlpha is the EWMA smoothing factor for newly observed
	// ratios. 0.2 == ~5-sample memory: responsive but not jumpy.
	calibrationAlpha = 0.2

	// calibrationMinSamples is how many observations a (provider,model)
	// needs before its learned ratio is trusted over the heuristic.
	calibrationMinSamples = 3

	// Sane clamps on the learned chars/token ratio. Real tokenizers sit
	// well inside this; the clamp only guards against a bogus usage report
	// (e.g. a proxy returning 0 or a nonsense token count).
	minCharsPerToken = 1.5
	maxCharsPerToken = 12.0
)

// tokenCalibrator maintains a smoothed chars-per-token ratio per
// provider/model, learned from real provider usage reports. Safe for
// concurrent use.
type tokenCalibrator struct {
	mu      sync.RWMutex
	entries map[string]*calibEntry
}

type calibEntry struct {
	ratio   float64
	samples int
}

var globalCalibrator = &tokenCalibrator{entries: make(map[string]*calibEntry)}

func calibKey(provider, model string) string { return provider + "\x00" + model }

// ratio returns the learned chars-per-token for a provider/model and
// whether it is trusted (has enough samples). Read-locked + fast.
func (c *tokenCalibrator) ratio(provider, model string) (float64, bool) {
	if provider == "" && model == "" {
		return 0, false
	}
	c.mu.RLock()
	e := c.entries[calibKey(provider, model)]
	c.mu.RUnlock()
	if e == nil || e.samples < calibrationMinSamples || e.ratio <= 0 {
		return 0, false
	}
	return e.ratio, true
}

// observe folds one real usage report (characters sent → prompt tokens
// counted) into the per-model EWMA. Ignores nonsensical inputs so a bad
// report can't poison the estimate.
func (c *tokenCalibrator) observe(provider, model string, chars, promptTokens int) {
	if promptTokens <= 0 || chars <= 0 || (provider == "" && model == "") {
		return
	}
	r := float64(chars) / float64(promptTokens)
	if r < minCharsPerToken {
		r = minCharsPerToken
	} else if r > maxCharsPerToken {
		r = maxCharsPerToken
	}

	key := calibKey(provider, model)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		c.entries[key] = &calibEntry{ratio: r, samples: 1}
		return
	}
	// EWMA: new = alpha*observed + (1-alpha)*previous.
	e.ratio = calibrationAlpha*r + (1-calibrationAlpha)*e.ratio
	e.samples++
}

// RecordTokenUsage folds a provider's reported prompt-token count into the
// calibration model. Providers call this after a Chat/StreamChat with the
// total characters they sent and the prompt_tokens the API reported. No-op
// on missing data, so providers that don't report usage simply keep using
// the heuristic. Safe for concurrent use.
func RecordTokenUsage(provider, model string, promptChars, promptTokens int) {
	globalCalibrator.observe(provider, model, promptChars, promptTokens)
}

// charsInMessages sums the rune length of all message contents (+ a small
// per-message framing constant) — the "characters we sent" side of the
// calibration ratio, computed consistently across providers.
func charsInMessages(messages []ChatMessage) int {
	total := 0
	for _, m := range messages {
		total += len([]rune(m.Content)) + len([]rune(m.Role)) + 4
	}
	return total
}
