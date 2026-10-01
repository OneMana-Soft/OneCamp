package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A fake provider that refuses prompts above a token ceiling the way real providers do,
// so the rescue is exercised against the behaviour it exists for rather than a mock that
// simply counts calls.
type overflowingProvider struct {
	limit    int         // refuse when the estimated prompt exceeds this
	lim      ModelLimits // how to estimate
	attempts [][]ChatMessage
}

func (p *overflowingProvider) Chat(_ context.Context, msgs []ChatMessage, _ ChatOptions) (string, error) {
	p.attempts = append(p.attempts, msgs)
	if p.lim.EstimateMessagesTokens(msgs) > p.limit {
		// Real wording from a real provider, matched by IsContextOverflow.
		return "", fmt.Errorf("status 400: This model's maximum context length is %d tokens, however you requested more", p.limit)
	}
	return "answered", nil
}

func (p *overflowingProvider) ChatStream(_ context.Context, msgs []ChatMessage, _ ChatOptions) (<-chan StreamChunk, error) {
	p.attempts = append(p.attempts, msgs)
	if p.lim.EstimateMessagesTokens(msgs) > p.limit {
		// Anthropic's shape, which also carries the limit — providers state it on the
		// streaming path too, so the stub must not be quietly easier than reality.
		return nil, fmt.Errorf("prompt is too long: %d tokens > %d maximum",
			p.lim.EstimateMessagesTokens(msgs), p.limit)
	}
	ch := make(chan StreamChunk, 1)
	close(ch)
	return ch, nil
}

func (p *overflowingProvider) ProviderName() ProviderType { return ProviderOpenAICompatible }

func longConversation(lim ModelLimits) []ChatMessage {
	msgs := []ChatMessage{{Role: roleSystem, Content: "You are a helpful assistant. Follow the rules."}}
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			ChatMessage{Role: roleUser, Content: strings.Repeat("an earlier question with plenty of detail. ", 40)},
			ChatMessage{Role: roleAssistant, Content: strings.Repeat("an earlier answer with plenty of detail. ", 40)},
		)
	}
	msgs = append(msgs, ChatMessage{Role: roleUser, Content: "So what should we do about the launch?"})
	return msgs
}

// The headline behaviour: a provider rejection for an over-long prompt becomes a
// successful answer rather than an error the user cannot act on.
func TestChatWithRescueRecoversFromOverflow(t *testing.T) {
	lim := ModelLimits{Provider: "openai_compatible", Model: "m", ContextWindow: 8192}
	p := &overflowingProvider{limit: 3000, lim: lim}
	ctx := WithModelLimits(context.Background(), lim)

	out, err := ChatWithRescue(ctx, p, longConversation(lim), ChatOptions{MaxTokens: 512})
	if err != nil {
		t.Fatalf("rescue should have recovered, got %v", err)
	}
	if out != "answered" {
		t.Errorf("unexpected answer %q", out)
	}
	if len(p.attempts) < 2 {
		t.Fatalf("expected a retry, got %d attempt(s)", len(p.attempts))
	}
	// The retry must be genuinely smaller, and must still carry the instructions and
	// the actual question — a shorter prompt that lost the question is not a recovery.
	first, last := p.attempts[0], p.attempts[len(p.attempts)-1]
	if lim.EstimateMessagesTokens(last) >= lim.EstimateMessagesTokens(first) {
		t.Error("the retry was not smaller than the prompt that was refused")
	}
	if last[0].Role != roleSystem || !strings.Contains(last[0].Content, "Follow the rules") {
		t.Error("the system prompt must survive intact: obeying truncated instructions is worse than failing")
	}
	if !strings.Contains(last[len(last)-1].Content, "launch") {
		t.Error("the user's actual question must survive")
	}
}

// Streaming gets the same treatment, and may: the rejection happens when the request is
// made, before any delta reaches a screen, so there is nothing half-rendered to undo.
func TestChatStreamWithRescueRecoversFromOverflow(t *testing.T) {
	lim := ModelLimits{Provider: "openai_compatible", Model: "m", ContextWindow: 8192}
	p := &overflowingProvider{limit: 3000, lim: lim}
	ctx := WithModelLimits(context.Background(), lim)

	ch, err := ChatStreamWithRescue(ctx, p, longConversation(lim), ChatOptions{MaxTokens: 512})
	if err != nil {
		t.Fatalf("stream rescue should have recovered, got %v", err)
	}
	if ch == nil {
		t.Error("expected a stream channel")
	}
	if len(p.attempts) < 2 {
		t.Errorf("expected a retry, got %d", len(p.attempts))
	}
}

// Errors that are NOT overflow must pass straight through, untouched and un-retried.
// Retrying a 401 or a rate limit here would duplicate work the callers already handle
// (and the breaker/fallback logic lives with them, not here).
func TestChatWithRescueLeavesOtherErrorsAlone(t *testing.T) {
	lim := WorkspaceLimits()
	for _, msg := range []string{"status 401 unauthorized", "connection refused", "rate limit exceeded"} {
		p := &countingProvider{err: errors.New(msg)}
		_, err := ChatWithRescue(WithModelLimits(context.Background(), lim), p, longConversation(lim), ChatOptions{})
		if err == nil || !strings.Contains(err.Error(), strings.Split(msg, " ")[0]) {
			t.Errorf("%q must be returned unchanged, got %v", msg, err)
		}
		if p.calls != 1 {
			t.Errorf("%q must not be retried, saw %d calls", msg, p.calls)
		}
	}
}

// A prompt that cannot be shrunk must fail ONCE and stop, not loop. The floor is the
// system prompt plus the question; below that there is no request left to send.
func TestChatWithRescueStopsWhenItCannotShrink(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 8192}
	// A single system message larger than anything the provider will accept.
	msgs := []ChatMessage{{Role: roleSystem, Content: strings.Repeat("rules. ", 5000)}}
	p := &overflowingProvider{limit: 10, lim: lim}

	_, err := ChatWithRescue(WithModelLimits(context.Background(), lim), p, msgs, ChatOptions{})
	if err == nil {
		t.Fatal("an unshrinkable prompt must still fail")
	}
	if !IsContextOverflow(err) {
		t.Errorf("the original overflow error must be preserved, got %v", err)
	}
	if len(p.attempts) > 1+maxOverflowRescues {
		t.Errorf("bounded retries exceeded: %d attempts", len(p.attempts))
	}
}

// ShrinkToBudget's contract, directly.
func TestShrinkToBudgetPreservesInstructionsAndQuestion(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 8192}
	msgs := longConversation(lim)
	full := lim.EstimateMessagesTokens(msgs)

	out, changed := lim.ShrinkToBudget(msgs, full/3)
	if !changed {
		t.Fatal("a conversation three times the budget must shrink")
	}
	if got := lim.EstimateMessagesTokens(out); got > full/3 {
		t.Errorf("shrunk to %d tokens, over the %d budget", got, full/3)
	}
	if out[0].Role != roleSystem {
		t.Error("system message must lead the result")
	}
	if !strings.Contains(out[len(out)-1].Content, "launch") {
		t.Error("the final question must survive")
	}
	// A tool result may never head the list — providers reject an orphaned one.
	for i, m := range out {
		if i > 0 && out[i-1].Role == roleSystem && m.Role == "tool" {
			t.Error("a tool message must never head the conversation body")
		}
	}
	// Already fitting => untouched, so the rescue is a no-op on the happy path.
	if _, changed := lim.ShrinkToBudget(msgs, full+1000); changed {
		t.Error("a conversation that already fits must not be modified")
	}
}

// The user-facing message must not tell someone to retry, because an identical retry
// overflows identically. This is the defect the FriendlyProviderError default caused.
func TestOverflowMessageDoesNotAdviseARetry(t *testing.T) {
	msg := FriendlyProviderError(errors.New("status 400: context_length_exceeded"))
	if msg == "" {
		t.Fatal("overflow must have a friendly message")
	}
	if strings.Contains(strings.ToLower(msg), "try again") {
		t.Errorf("overflow advice must not be 'try again' — it cannot work: %q", msg)
	}
	for _, want := range []string{"too long", "context window"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("message should mention %q, got %q", want, msg)
		}
	}
	// And a non-overflow error must still get its own message, not this one.
	if FriendlyProviderError(errors.New("connection refused")) == msg {
		t.Error("overflow advice leaked onto an unrelated error")
	}
}

// A counting stub for the pass-through cases.
type countingProvider struct {
	err   error
	calls int
}

func (p *countingProvider) Chat(context.Context, []ChatMessage, ChatOptions) (string, error) {
	p.calls++
	return "", p.err
}
func (p *countingProvider) ChatStream(context.Context, []ChatMessage, ChatOptions) (<-chan StreamChunk, error) {
	p.calls++
	return nil, p.err
}
func (p *countingProvider) ProviderName() ProviderType { return ProviderOllama }

// The provider usually states its real limit, and that number is the only
// authoritative one available at this point — our configured window is by definition
// wrong when this fires. Both common shapes put the LIMIT below the requested size, so
// the smallest plausible integer is the limit.
func TestOverflowLimitParsedFromProviderText(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		// OpenAI / vLLM / Groq shape.
		{"status 400: This model's maximum context length is 8192 tokens, however you requested 10000 tokens", 8192},
		// Anthropic shape.
		{"prompt is too long: 210000 tokens > 200000 maximum", 200000},
		// Status codes and small error codes must not be mistaken for a window.
		{"status 400: context_length_exceeded", 0},
		{"error 429 too many requests", 0},
		// No numbers at all is the honest zero, which sends the caller to the
		// geometric fallback.
		{"prompt is too long", 0},
	}
	for _, c := range cases {
		if got := overflowLimitFromError(errors.New(c.text)); got != c.want {
			t.Errorf("%q: parsed %d, want %d", c.text, got, c.want)
		}
	}
	if overflowLimitFromError(nil) != 0 {
		t.Error("nil error must parse to 0")
	}
}

// A stated limit must be used in preference to guessing, because guessing converges
// slowly when the configured window is far out — a workspace set to 128k while a member
// runs an 8k local model is a 16x error, and halving from there costs four refused
// round trips.
func TestRescueTargetPrefersTheStatedLimit(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 128000}
	refused := longConversation(lim)

	guessed := rescueTarget(lim, refused, errors.New("prompt is too long"), 1024)

	// A stated limit BELOW the geometric guess must tighten the target — this is the
	// case geometric halving handles badly, and the reason parsing is worth doing.
	stated := rescueTarget(lim, refused, errors.New("maximum context length is 2048 tokens"), 1024)
	if stated >= guessed {
		t.Errorf("a stated limit tighter than the guess must win: stated=%d guessed=%d", stated, guessed)
	}
	if stated > 2048 {
		t.Errorf("target %d exceeds the limit the provider stated (2048)", stated)
	}
	if stated <= 0 {
		t.Errorf("target must stay positive, got %d", stated)
	}

	// A stated limit ABOVE the geometric guess must not loosen it. The guess is already
	// known to be smaller than something that failed, so widening back out would send a
	// prompt larger than necessary and risk another refusal.
	loose := rescueTarget(lim, refused, errors.New("maximum context length is 999999 tokens"), 1024)
	if loose > guessed {
		t.Errorf("a stated limit above the guess must not loosen the target: stated=%d guessed=%d", loose, guessed)
	}
}

// The stated limit must reach the ACTUAL rescue, not just rescueTarget in isolation.
//
// This test exists because it was missing, and its absence hid a real defect:
// rescueTarget was written, unit-tested and never called. ChatWithRescue kept halving
// geometrically, so the provider-stated limit — the one authoritative number available
// when this fires — was parsed and thrown away. Unit-testing a helper proves the helper;
// only a behavioural test proves it is wired.
//
// Constructed so geometric halving CANNOT converge within the retry budget while the
// stated limit can. The configured window is far too large (the misconfiguration this
// whole path exists for), the conversation is ~10k tokens, and the provider's real limit
// is 1500. Halving twice reaches ~2.5k and still fails; honouring the stated 1500
// succeeds on the first retry.
func TestRescueHonoursTheStatedLimitEndToEnd(t *testing.T) {
	lim := ModelLimits{Provider: "openai_compatible", Model: "small-window", ContextWindow: 128000}
	ctx := WithModelLimits(context.Background(), lim)
	p := &overflowingProvider{limit: 1500, lim: lim}

	out, err := ChatWithRescue(ctx, p, longConversation(lim), ChatOptions{MaxTokens: 512})
	if err != nil {
		t.Fatalf("the rescue must honour the limit the provider stated; geometric halving alone "+
			"cannot converge here and that is the point of parsing it: %v", err)
	}
	if out != "answered" {
		t.Errorf("unexpected answer %q", out)
	}
	// Prove it converged FAST — using the stated limit should land it in one retry,
	// where halving would need four. If this starts needing more attempts, the stated
	// limit has stopped being honoured even if the call still eventually succeeds.
	if len(p.attempts) > 2 {
		t.Errorf("expected the first retry to fit (stated limit honoured), took %d attempts", len(p.attempts))
	}
}

// Same shape for streaming, since it runs its own copy of the loop and could drift.
func TestStreamRescueHonoursTheStatedLimitEndToEnd(t *testing.T) {
	lim := ModelLimits{Provider: "openai_compatible", Model: "small-window", ContextWindow: 128000}
	ctx := WithModelLimits(context.Background(), lim)
	p := &overflowingProvider{limit: 1500, lim: lim}

	if _, err := ChatStreamWithRescue(ctx, p, longConversation(lim), ChatOptions{MaxTokens: 512}); err != nil {
		t.Fatalf("streaming rescue must honour the stated limit too: %v", err)
	}
	if len(p.attempts) > 2 {
		t.Errorf("expected the first retry to fit, took %d attempts", len(p.attempts))
	}
}
