package ai

import (
	"context"
	"strings"
	"testing"
)

// withRatio installs a known calibrated ratio for a model so a test controls the
// tokenizer instead of depending on whatever the process has learned, and removes it
// afterwards.
func withRatio(t *testing.T, provider, model string, ratio float64) {
	t.Helper()
	globalCalibrator.mu.Lock()
	globalCalibrator.entries[calibKey(provider, model)] = &calibEntry{ratio: ratio, samples: calibrationMinSamples}
	globalCalibrator.mu.Unlock()
	t.Cleanup(func() {
		globalCalibrator.mu.Lock()
		delete(globalCalibrator.entries, calibKey(provider, model))
		globalCalibrator.mu.Unlock()
	})
}

// 0 means inherit, and inheriting must land on the workspace window. This is the promise
// migration 140 makes to every existing deployment: adding the columns changes nothing
// until an admin fills one in. If this breaks, an upgrade silently re-sizes every prompt
// in every workspace.
func TestUnsetLimitsInheritTheWorkspaceWindow(t *testing.T) {
	ws := WorkspaceLimits()
	got := LimitsForModel("anthropic", "some-model", 0, 0)
	if got.ContextWindow != ws.ContextWindow {
		t.Errorf("an unset per-model window must inherit the workspace one: got %d, want %d",
			got.ContextWindow, ws.ContextWindow)
	}
	if got.MaxOutput != 0 {
		t.Errorf("an unset max-output must stay 0 so the caller's own reservation wins, got %d", got.MaxOutput)
	}
	if got.Provider != "anthropic" || got.Model != "some-model" {
		t.Errorf("the answering model must be recorded, got %q/%q", got.Provider, got.Model)
	}
}

// An admin-stated window wins over the workspace one, in BOTH directions — both were
// broken before and in opposite ways. A large pin was budgeted small, throwing away
// context that fitted (and on Ollama running the model small). A small pin was budgeted
// large, building prompts the model cannot accept.
func TestStatedWindowOverridesTheWorkspace(t *testing.T) {
	big := LimitsForModel("anthropic", "big-window", 200000, 0)
	if big.ContextWindow != 200000 {
		t.Errorf("stated window must win: got %d, want 200000", big.ContextWindow)
	}
	if big.Source != limitSourceModel {
		t.Errorf("source must record that the model stated it, got %q", big.Source)
	}
	// Smaller than the workspace must ALSO win — that is the half preventing overflow
	// rather than merely recovering wasted capacity.
	if small := LimitsForModel("ollama", "tiny", 4096, 0); small.ContextWindow != 4096 {
		t.Errorf("a stated window below the workspace one must still win: got %d, want 4096", small.ContextWindow)
	}
	// Below the floor is clamped, so a typo cannot collapse every budget to its minimum.
	if got := LimitsForModel("ollama", "m", 1, 0).ContextWindow; got < minContextWindow {
		t.Errorf("window must be floored at %d, got %d", minContextWindow, got)
	}
}

// A stated output ceiling is taken only when it is SMALLER than what the caller asked
// for. Asking a model for more than it produces does not get more output; it reserves
// window that could have carried context.
func TestStatedOutputCeilingTakesTheSmaller(t *testing.T) {
	lim := LimitsForModel("openai", "m", 128000, 4096)
	if got := lim.ResponseReserve(8192); got != 4096 {
		t.Errorf("the model's lower ceiling must win: got %d, want 4096", got)
	}
	if got := lim.ResponseReserve(1024); got != 1024 {
		t.Errorf("a caller asking for less must not be inflated to the ceiling: got %d, want 1024", got)
	}
	none := LimitsForModel("openai", "m", 128000, 0)
	if got := none.ResponseReserve(2048); got != 2048 {
		t.Errorf("with no stated ceiling the caller decides: got %d, want 2048", got)
	}
	// A ceiling below the DEFAULT reserve leaves more room for input, because the budget
	// stops holding back tokens the model will never produce. 4096 is above the 1024
	// default, so it correctly changes nothing there — only a genuinely small ceiling
	// should move the budget.
	tiny := LimitsForModel("openai", "m", 128000, 512)
	if tiny.AvailableInputTokens() <= none.AvailableInputTokens() {
		t.Errorf("a ceiling (512) below the default reserve (%d) should free input room: %d vs %d",
			responseTokenReserve, tiny.AvailableInputTokens(), none.AvailableInputTokens())
	}
	if lim.AvailableInputTokens() != none.AvailableInputTokens() {
		t.Error("a ceiling above the default reserve must not change the input budget")
	}
}

// The tokenizer ratio must be read for the model being MEASURED, not for whichever
// model the workspace defaults to.
//
// The calibrator always stored ratios per (provider, model) — correctly — but every
// read went through the workspace's active model, so a channel, agent or member on a
// different model was measured with the default model's tokenizer. Providers differ by
// 15-30% on the same text, and a single tokenizer change within one vendor's line has
// moved it by that much, which is enough to overflow a window the budget believed it
// was respecting.
func TestRatioIsReadPerModelNotPerWorkspace(t *testing.T) {
	dense := ModelLimits{Provider: "vendor-a", Model: "dense-tokenizer", ContextWindow: 8192}
	sparse := ModelLimits{Provider: "vendor-b", Model: "sparse-tokenizer", ContextWindow: 8192}
	withRatio(t, dense.Provider, dense.Model, 2.0)
	withRatio(t, sparse.Provider, sparse.Model, 8.0)

	const text = "the quick brown fox jumps over the lazy dog, repeatedly and at length"
	if d, s := dense.EstimateTokens(text), sparse.EstimateTokens(text); d <= s {
		t.Errorf("a denser tokenizer must yield MORE tokens for the same text: dense=%d sparse=%d", d, s)
	}
	if dense.CharsPerToken() != 2.0 || sparse.CharsPerToken() != 8.0 {
		t.Errorf("each model must read its own ratio, got dense=%.1f sparse=%.1f",
			dense.CharsPerToken(), sparse.CharsPerToken())
	}
}

// An uncalibrated model falls back to the static heuristic rather than to zero, which
// would divide by zero and make every budget meaningless.
func TestUncalibratedModelUsesTheHeuristic(t *testing.T) {
	lim := ModelLimits{Provider: "never", Model: "seen", ContextWindow: 8192}
	if got := lim.CharsPerToken(); got != float64(avgCharsPerToken) {
		t.Errorf("an uncalibrated model must use the %d-chars heuristic, got %.2f", avgCharsPerToken, got)
	}
	if lim.EstimateTokens("hello world") <= 0 {
		t.Error("an uncalibrated model must still produce a positive estimate")
	}
}

// Truncation must respect the budget it was given, under ANY tokenizer ratio.
//
// Regression test for measuring with one ratio and cutting with another: the old code
// estimated with the calibrated ratio but converted the surviving budget back to
// characters with a hardcoded 4, so every model whose real ratio was below 4 kept more
// text than the budget allowed and overflowed the limit it existed to enforce. Two
// numbers for one conversion is one too many.
func TestTruncationRespectsItsBudgetAtEveryRatio(t *testing.T) {
	body := strings.Repeat("func handle(w http.ResponseWriter, r *http.Request) { /* x */ }\n", 400)

	for _, ratio := range []float64{1.5, 2.5, 4.0, 8.0, 12.0} {
		lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 128000}
		withRatio(t, lim.Provider, lim.Model, ratio)

		const budget = 500
		out := lim.TruncateToTokenBudget(body, budget)
		if got := lim.EstimateTokens(out); got > budget {
			t.Errorf("ratio %.1f: truncated output is %d tokens, over its %d budget", ratio, got, budget)
		}
		if !strings.Contains(out, "truncated") {
			t.Errorf("ratio %.1f: truncation must be visible to the model", ratio)
		}
	}
}

// History trimming uses the same per-model ratio, so the two halves of a prompt are
// measured consistently.
func TestTrimHistoryRespectsItsBudget(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "trim", ContextWindow: 8192}
	withRatio(t, lim.Provider, lim.Model, 2.0)

	var history []ChatMessage
	for i := 0; i < 40; i++ {
		history = append(history, ChatMessage{Role: roleUser, Content: strings.Repeat("some earlier turn. ", 20)})
	}
	kept := lim.TrimHistoryToBudget(history, 400)
	if len(kept) == 0 {
		t.Fatal("some history should survive a 400-token budget")
	}
	if got := lim.EstimateMessagesTokens(kept); got > 400+8*len(kept) {
		t.Errorf("kept history is %d tokens, well over the 400 budget", got)
	}
	// Trimming keeps the NEWEST turns: the tail is what the next answer depends on.
	if kept[len(kept)-1].Content != history[len(history)-1].Content {
		t.Error("the most recent turn must be the one kept")
	}
}

// Budgets are carved out of the window and must never exceed it.
func TestBudgetsFitInsideTheWindow(t *testing.T) {
	for _, w := range []int{2048, 8192, 32000, 128000} {
		lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: w}
		if lim.ContextBudget()+lim.HistoryBudget() > w {
			t.Errorf("window %d: budgets (%d + %d) exceed it", w, lim.ContextBudget(), lim.HistoryBudget())
		}
		if lim.ContextBudget() <= 0 || lim.HistoryBudget() <= 0 {
			t.Errorf("window %d: budgets must stay positive, got %d and %d", w, lim.ContextBudget(), lim.HistoryBudget())
		}
		if lim.CompactionTriggerTokens(1024) > lim.ConversationInputBudget(1024) {
			t.Errorf("window %d: compaction must trigger before the input budget is spent", w)
		}
	}
	// A larger window must give more room. Trivial-sounding, and it is exactly what
	// did not hold when every budget read one workspace-wide number.
	small := ModelLimits{Provider: "p", Model: "s", ContextWindow: 8192}
	big := ModelLimits{Provider: "p", Model: "b", ContextWindow: 128000}
	if big.ContextBudget() <= small.ContextBudget() {
		t.Error("a larger window must give a larger context budget")
	}
}

// A context with no limits attached must budget exactly as the workspace does, so any
// path not yet carrying limits keeps its current behaviour instead of degrading to a
// zero window (which would silently drop all context).
func TestContextWithoutLimitsFallsBackToWorkspace(t *testing.T) {
	if got, ws := LimitsFrom(context.Background()), WorkspaceLimits(); got.ContextWindow != ws.ContextWindow {
		t.Errorf("a bare context must yield the workspace window: got %d, want %d", got.ContextWindow, ws.ContextWindow)
	}
	if LimitsFrom(nil).ContextWindow <= 0 {
		t.Error("even a nil context must yield a usable window")
	}
	// A zero value must be refused by the setter, or it would poison every budget
	// downstream of the attachment.
	if LimitsFrom(WithModelLimits(context.Background(), ModelLimits{})).ContextWindow <= 0 {
		t.Error("attaching an empty ModelLimits must not produce a zero window")
	}
}

// Round trip, so budget code downstream of an attachment measures against the model
// the resolver actually chose.
func TestLimitsSurviveTheContext(t *testing.T) {
	want := ModelLimits{Provider: "anthropic", Model: "some-model", ContextWindow: 200000}
	got := LimitsFrom(WithModelLimits(context.Background(), want))
	if got.ContextWindow != want.ContextWindow || got.Model != want.Model || got.Provider != want.Provider {
		t.Errorf("limits did not survive the context: got %+v, want %+v", got, want)
	}
}

// The model's window must reach the CLIENT, not just the budget.
//
// This is the severe half of the original defect and the half no budget test can catch.
// Ollama applies num_ctx at construction, so a model an admin allowed with a 128k window
// was built with the workspace's — often 8192 — and was genuinely RUN small. The prompt
// budget being wrong wasted capacity; this threw it away inside the provider, where
// nothing downstream could see it had happened.
func TestResolvedClientIsBuiltWithTheModelsOwnWindow(t *testing.T) {
	cfg := &AIConfig{ContextWindowTokens: 8192}
	ep := Endpoint{Kind: ProviderOllama, BaseURL: "http://localhost:11434", Model: "big-model"}

	mc := newModelResolver()
	rm, err := mc.get("k|big-model", ep, cfg, func() ModelLimits {
		return LimitsForModel("ollama", "big-model", 131072, 0)
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	op, ok := rm.llm.(*OllamaProvider)
	if !ok {
		t.Fatalf("expected an Ollama provider, got %T", rm.llm)
	}
	if op.numCtx != 131072 {
		t.Errorf("the client must run at the MODEL's window: num_ctx=%d, want 131072 "+
			"(the workspace is 8192, and using it here is the bug this guards)", op.numCtx)
	}
	if rm.limits.ContextWindow != 131072 {
		t.Errorf("cached limits must match the built client, got %d", rm.limits.ContextWindow)
	}

	// A cache hit must not re-resolve: that lookup reads the database, and paying for it
	// per request is why it is a function rather than a value.
	again, err := mc.get("k|big-model", ep, cfg, func() ModelLimits {
		t.Error("limitsFor must not be called on a cache hit — that would be a DB read per request")
		return ModelLimits{ContextWindow: 1}
	})
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if again.limits.ContextWindow != 131072 {
		t.Errorf("cache hit returned different limits: %d", again.limits.ContextWindow)
	}
}

// nil means "workspace window", preserving the behaviour of paths that legitimately have
// no allowlist row — the env-configured local fallback model.
func TestNilLimitsResolverUsesTheWorkspaceWindow(t *testing.T) {
	cfg := &AIConfig{ContextWindowTokens: 16384}
	ep := Endpoint{Kind: ProviderOllama, BaseURL: "http://localhost:11434", Model: "fallback"}

	rm, err := newModelResolver().get("fallback|x", ep, cfg, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	op, ok := rm.llm.(*OllamaProvider)
	if !ok || op.numCtx != 16384 {
		t.Errorf("a nil limits resolver must fall back to the workspace window 16384, got %v", rm.llm)
	}
	// The limits must still name the model, so estimation uses its tokenizer.
	if rm.limits.Model != "fallback" {
		t.Errorf("limits must identify the resolved model, got %q", rm.limits.Model)
	}
}

func TestPackageHelpersAgreeWithWorkspaceLimits(t *testing.T) {
	ws := WorkspaceLimits()
	if ContextTokenBudget() != ws.ContextBudget() {
		t.Errorf("ContextTokenBudget=%d but WorkspaceLimits says %d", ContextTokenBudget(), ws.ContextBudget())
	}
	if EstimateTokens("some sample text") != ws.EstimateTokens("some sample text") {
		t.Error("EstimateTokens must agree with WorkspaceLimits().EstimateTokens")
	}
}
