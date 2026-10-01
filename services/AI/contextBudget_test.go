package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty string should be 0 tokens, got %d", got)
	}
	// ~4 chars/token, rounded up.
	if got := EstimateTokens("abcd"); got != 1 {
		t.Errorf("4 chars should be 1 token, got %d", got)
	}
	if got := EstimateTokens("abcde"); got != 2 {
		t.Errorf("5 chars should round up to 2 tokens, got %d", got)
	}
	// Multibyte: counted by runes, not bytes (so we don't over-count).
	s := strings.Repeat("é", 8) // 8 runes, 16 bytes
	if got := EstimateTokens(s); got != 2 {
		t.Errorf("8 runes should be 2 tokens (rune-based), got %d", got)
	}
}

func TestTruncateToTokenBudget(t *testing.T) {
	// Under budget → unchanged.
	s := "short context"
	if got := TruncateToTokenBudget(s, 1000); got != s {
		t.Errorf("under-budget content should be unchanged, got %q", got)
	}

	// Zero budget → empty.
	if got := TruncateToTokenBudget(s, 0); got != "" {
		t.Errorf("zero budget should yield empty, got %q", got)
	}

	// Over budget → truncated, fits the budget, and carries the marker.
	big := strings.Repeat("word ", 5000) // ~25k chars ≈ 6250 tokens
	out := TruncateToTokenBudget(big, 100)
	if EstimateTokens(out) > 100 {
		t.Errorf("truncated output exceeds budget: %d tokens", EstimateTokens(out))
	}
	if !strings.Contains(out, "truncated") {
		tail := out
		if len(out) > 60 {
			tail = out[len(out)-60:]
		}
		t.Errorf("truncated output should include the truncation marker, got tail: %q", tail)
	}
}

func TestTokenCalibrator(t *testing.T) {
	c := &tokenCalibrator{entries: make(map[string]*calibEntry)}

	// Before any samples → not trusted.
	if _, ok := c.ratio("openai", "gpt-4o"); ok {
		t.Error("uncalibrated model should not be trusted")
	}

	// Feed consistent ~3 chars/token observations (e.g. code-heavy content).
	for i := 0; i < calibrationMinSamples; i++ {
		c.observe("openai", "gpt-4o", 300, 100)
	}
	r, ok := c.ratio("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected trusted ratio after min samples")
	}
	if r < 2.5 || r > 3.5 {
		t.Errorf("expected ratio near 3.0, got %.2f", r)
	}

	// A different model is independent.
	if _, ok := c.ratio("anthropic", "claude"); ok {
		t.Error("unrelated model should remain uncalibrated")
	}
}

func TestTokenCalibrator_ClampsAndIgnoresGarbage(t *testing.T) {
	c := &tokenCalibrator{entries: make(map[string]*calibEntry)}

	// Garbage inputs are ignored (no entry created).
	c.observe("p", "m", 0, 100)
	c.observe("p", "m", 100, 0)
	c.observe("", "", 100, 100)
	if _, ok := c.ratio("p", "m"); ok {
		t.Error("garbage observations must not create a trusted entry")
	}

	// An absurd ratio (1000 chars / 1 token) is clamped to maxCharsPerToken.
	for i := 0; i < calibrationMinSamples; i++ {
		c.observe("p", "m", 1000, 1)
	}
	r, ok := c.ratio("p", "m")
	if !ok {
		t.Fatal("expected trusted entry")
	}
	if r > maxCharsPerToken+0.001 {
		t.Errorf("ratio must be clamped to <= %.1f, got %.2f", maxCharsPerToken, r)
	}
}

func TestTokenCalibrator_EWMAConverges(t *testing.T) {
	c := &tokenCalibrator{entries: make(map[string]*calibEntry)}
	// Start at 4.0, then feed a stream of 2.0 observations; the EWMA should
	// move toward 2.0 without overshooting below it.
	c.observe("p", "m", 400, 100) // 4.0
	for i := 0; i < 50; i++ {
		c.observe("p", "m", 200, 100) // 2.0
	}
	r, _ := c.ratio("p", "m")
	if r < 2.0-0.01 || r > 2.5 {
		t.Errorf("EWMA should converge near 2.0, got %.3f", r)
	}
}

func TestCharsInMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "abc"},
		{Role: "user", Content: "héllo"}, // 5 runes
	}
	got := charsInMessages(msgs)
	// 3 + 6 ("system") + 4  +  5 + 4 ("user") + 4 = 26
	if got != 26 {
		t.Errorf("charsInMessages = %d, want 26", got)
	}
}

func TestEffectiveContextWindow(t *testing.T) {
	// Admin-set value wins.
	c := &AIConfig{ContextWindowTokens: 32768, OllamaNumCtx: 8192}
	if got := c.EffectiveContextWindow(); got != 32768 {
		t.Errorf("admin value should win, got %d", got)
	}

	// 0 admin → fall back to env-derived OllamaNumCtx.
	c = &AIConfig{ContextWindowTokens: 0, OllamaNumCtx: 16384}
	if got := c.EffectiveContextWindow(); got != 16384 {
		t.Errorf("should fall back to OllamaNumCtx, got %d", got)
	}

	// Both unset → default.
	c = &AIConfig{}
	if got := c.EffectiveContextWindow(); got != defaultContextWindow {
		t.Errorf("should fall back to default %d, got %d", defaultContextWindow, got)
	}

	// Below floor → floored.
	c = &AIConfig{ContextWindowTokens: 100}
	if got := c.EffectiveContextWindow(); got != minContextWindow {
		t.Errorf("tiny value should be floored to %d, got %d", minContextWindow, got)
	}
}

// --- Conversation compaction ---
//
// The invariant that matters most: a compacted conversation must stay
// PROVIDER-LEGAL. The tail may never begin with a `tool` result, because that
// would orphan it from the assistant turn that requested it and every native
// tool-calling provider rejects that with a 400.

// convo builds a realistic native-protocol tool loop: system + user, then n
// steps of (assistant with a tool call) + (tool result).
func convo(n int, resultChars int) []ChatMessage {
	msgs := []ChatMessage{
		{Role: "system", Content: "you are an agent"},
		{Role: "user", Content: "summarise this week's commits in akashc777/onecamp-fe"},
	}
	for i := 0; i < n; i++ {
		id := "call_" + string(rune('a'+i))
		msgs = append(msgs,
			ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{
				ID: id, Name: "github_list_commits", Arguments: `{"repo":"akashc777/onecamp-fe"}`,
			}}},
			ChatMessage{Role: "tool", ToolCallID: id, Name: "github_list_commits", Content: strings.Repeat("commit ", resultChars/7)},
		)
	}
	return msgs
}

func TestPickCompactionBoundary_NeverHeadsWithAToolResult(t *testing.T) {
	msgs := convo(8, 700)
	for _, keep := range []int{64, 200, 600, 1500, 4000} {
		b := PickCompactionBoundary(msgs, 1, keep)
		if b < 0 {
			continue
		}
		if b >= len(msgs) {
			t.Fatalf("keep=%d: boundary %d must leave at least one message", keep, b)
		}
		if role := msgs[b].Role; role != "user" && role != "assistant" {
			t.Fatalf("keep=%d: boundary landed on role %q — a tail may only start at a user/assistant turn", keep, role)
		}
		// Every kept tool result must still be preceded by its assistant turn.
		open := map[string]bool{}
		for _, m := range msgs[b:] {
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
			if m.Role == "tool" && !open[m.ToolCallID] {
				t.Fatalf("keep=%d: tool result %s survived without its assistant turn", keep, m.ToolCallID)
			}
		}
	}
}

func TestPickCompactionBoundary_NothingWorthFolding(t *testing.T) {
	msgs := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}
	if b := PickCompactionBoundary(msgs, 1, 10000); b != -1 {
		t.Errorf("a two-message conversation has nothing to fold, got boundary %d", b)
	}
	if b := PickCompactionBoundary(msgs, 1, 0); b != -1 {
		t.Errorf("a zero keep budget must not produce a boundary, got %d", b)
	}
}

func TestCompactConversation_ShrinksAndKeepsSystemPlusTail(t *testing.T) {
	msgs := convo(12, 1200)
	before := EstimateMessagesTokens(msgs)
	f := &fakeLLM{outs: []string{"## Request and intent\nsummarise commits\n\n## Current work and next step\nreport"}}

	res := CompactConversation(context.Background(), f, msgs, CompactOptions{
		ResponseTokens: 2048,
		KeepTokens:     400,
		SyntheticUser:  func(m ChatMessage) bool { return false },
		Model:          "test-model",
	})
	if !res.Compacted || res.State == nil {
		t.Fatalf("expected a compaction (before=%d tokens, err=%v)", before, res.SummaryErr)
	}
	if f.calls != 1 {
		t.Errorf("expected exactly one summarizer call, got %d", f.calls)
	}
	if res.Messages[0].Role != "system" || res.Messages[0].Content != "you are an agent" {
		t.Error("the system prompt must survive compaction untouched")
	}
	if !IsCompactedBlock(res.Messages[1]) {
		t.Fatalf("expected the compaction block right after the system prompt, got %+v", res.Messages[1])
	}
	if !strings.Contains(res.Messages[1].Content, "summarise commits") {
		t.Error("the block must carry the summary text")
	}
	after := EstimateMessagesTokens(res.Messages)
	if after >= before {
		t.Errorf("compaction must shrink the prompt: %d -> %d", before, after)
	}
	if res.State.TokensAfter != after || res.State.TokensBefore != before {
		t.Errorf("state must report the real sizes: got %d->%d want %d->%d",
			res.State.TokensBefore, res.State.TokensAfter, before, after)
	}
	// The newest turn is always kept verbatim.
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "tool" || last.Content != msgs[len(msgs)-1].Content {
		t.Error("the most recent turn must be preserved verbatim")
	}
}

func TestCompactConversation_SummarizerFailureKeepsMechanicalState(t *testing.T) {
	msgs := convo(10, 1200)
	f := &fakeLLM{errs: []error{errors.New("provider down")}}

	res := CompactConversation(context.Background(), f, msgs, CompactOptions{ResponseTokens: 2048, KeepTokens: 400})
	if !res.Compacted || res.State == nil {
		t.Fatal("a failed summarizer must still compact (mechanical state is free)")
	}
	if res.SummaryErr == nil || !res.State.Trimmed {
		t.Error("the state must record that no summary was available")
	}
	block := res.Messages[1].Content
	if !strings.Contains(block, "no summary of them is available") {
		t.Error("the block must be honest that detail was lost")
	}
	if !strings.Contains(block, "github_list_commits") {
		t.Error("mechanical working state (the tools that ran) must survive without a summarizer")
	}
}

func TestCompactConversation_DeclinesWhenNothingToGain(t *testing.T) {
	// A short conversation: folding it would cost a model call and save nothing.
	msgs := []ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	f := &fakeLLM{outs: []string{"summary"}}
	res := CompactConversation(context.Background(), f, msgs, CompactOptions{ResponseTokens: 2048})
	if res.Compacted {
		t.Error("must not compact a conversation with nothing worth folding")
	}
	if f.calls != 0 {
		t.Errorf("must not spend a summarizer call on a trivial conversation, got %d calls", f.calls)
	}
	if len(res.Messages) != len(msgs) {
		t.Error("declined compaction must return the conversation unchanged")
	}
}

func TestCompactConversation_RepeatFoldsPriorSummaryOnce(t *testing.T) {
	msgs := convo(12, 1200)
	f := &fakeLLM{outs: []string{"FIRST SUMMARY", "SECOND SUMMARY"}}
	first := CompactConversation(context.Background(), f, msgs, CompactOptions{ResponseTokens: 2048, KeepTokens: 400})
	if !first.Compacted {
		t.Fatal("first compaction should have happened")
	}
	// Grow the conversation again, then compact a second time.
	grown := append(append([]ChatMessage{}, first.Messages...), convo(10, 1200)[2:]...)
	second := CompactConversation(context.Background(), f, grown, CompactOptions{
		Prior: first.State, ResponseTokens: 2048, KeepTokens: 400,
	})
	if !second.Compacted || second.State == nil {
		t.Fatal("second compaction should have happened")
	}
	if second.State.Rounds != 2 {
		t.Errorf("rounds should accumulate, got %d", second.State.Rounds)
	}
	if second.State.Folded <= first.State.Folded {
		t.Errorf("folded count should accumulate: %d -> %d", first.State.Folded, second.State.Folded)
	}
	// Exactly one block, never a block inside a block.
	blocks := 0
	for _, m := range second.Messages {
		if IsCompactedBlock(m) {
			blocks++
		}
	}
	if blocks != 1 {
		t.Errorf("a compacted conversation must carry exactly one block, got %d", blocks)
	}
	if strings.Count(second.Messages[1].Content, "SECOND SUMMARY") != 1 {
		t.Error("the block must carry the newest summary")
	}
}

func TestExtractUserIntentAndWorkingState(t *testing.T) {
	span := []ChatMessage{
		{Role: "user", Content: "  open a PR   that fixes the typo  "},
		{Role: "user", Content: "Tool results:\ngithub_list_commits -> ok"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "1", Name: "github_list_commits", Arguments: `{"repo":"akashc777/oneCamp"}`},
			{ID: "2", Name: "code_pr", Arguments: `{"instruction":"fix the typo"}`},
		}},
		{Role: "tool", ToolCallID: "1", Name: "github_list_commits", Content: "3 commits"},
		{Role: "tool", ToolCallID: "2", Name: "code_pr", Content: "error: repository not linked"},
	}
	synthetic := func(m ChatMessage) bool {
		return m.Role == "user" && strings.HasPrefix(m.Content, "Tool results:")
	}
	users := ExtractUserIntent(span, synthetic)
	if len(users) != 1 || users[0] != "open a PR that fixes the typo" {
		t.Errorf("expected one whitespace-normalised human message, got %#v", users)
	}

	ws := ExtractWorkingState(span)
	if !strings.Contains(ws, "akashc777/oneCamp") {
		t.Error("working state should name what was touched")
	}
	if !strings.Contains(ws, "FAILED") || !strings.Contains(ws, "code_pr") {
		t.Errorf("a failed call must be recorded as failed, got:\n%s", ws)
	}
	if strings.Contains(ws, "Actions that ran: code_pr") {
		t.Error("a failed write must not be listed as an action that ran")
	}
}

func TestIsContextOverflow(t *testing.T) {
	for _, msg := range []string{
		"openai: API returned status 400: {\"error\":{\"code\":\"context_length_exceeded\"}}",
		"groq: Please reduce the length of the messages",
		"anthropic: prompt is too long: 210000 tokens > 200000",
	} {
		if !IsContextOverflow(errors.New(msg)) {
			t.Errorf("should be recognised as a context overflow: %q", msg)
		}
	}
	for _, msg := range []string{"", "429 rate limit exceeded", "connection refused"} {
		if IsContextOverflow(errors.New(msg)) {
			t.Errorf("must not be treated as a context overflow: %q", msg)
		}
	}
	if IsContextOverflow(nil) {
		t.Error("nil error is not an overflow")
	}
}

func TestCompactionBudgets_Ordering(t *testing.T) {
	ws := WorkspaceLimits()
	trigger := ws.CompactionTriggerTokens(2048)
	keep := ws.CompactionKeepTokens(2048)
	budget := ws.ConversationInputBudget(2048)
	if trigger > budget {
		t.Errorf("trigger (%d) must fire before the input budget (%d) is spent", trigger, budget)
	}
	if keep >= trigger {
		t.Errorf("verbatim tail (%d) must be smaller than the trigger (%d) or compaction gains nothing", keep, trigger)
	}
	if s := ws.compactionSummaryTokens(2048); s <= 0 || s > compactionMaxSummaryTokens {
		t.Errorf("summary ceiling out of range: %d", s)
	}
}
