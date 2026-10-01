package ai

// Recovery from a prompt the model refuses as too long.
//
// Every budget in this package is an ESTIMATE. The window can be misconfigured, the
// tokenizer ratio is learned rather than exact, and for the openai_compatible provider
// kind the real window is unknowable — an admin can point it at any endpoint, including
// a router that serves different models under one name. So "we budgeted correctly" is
// not something this system can guarantee, and the provider is the only component that
// knows the truth.
//
// When it says no, that is recoverable information, not a failure: shrink the prompt
// and ask again. The agent loop has done this since it shipped (IsContextOverflow +
// a summarising compaction). Every other caller — the interactive assistant, channel
// Q&A, doc actions, search answers, board tools — failed the request outright, and the
// error text did not even reach FriendlyError's list, so a user was told "please try
// again" about the one failure that is guaranteed to repeat identically.
//
// This is the generic net for those callers. It is deliberately cruder than the agent's
// version: no summariser call, no state to carry, no persistence — just drop the oldest
// turns and clip what is left. A one-shot request has no run to protect, and a fast
// degraded answer beats a slow perfect one that arrives after two extra model calls.

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

const (
	// maxOverflowRescues bounds the retries. Two is enough for a mispredicted
	// tokenizer or a window overstated by an admin; beyond that the prompt has a
	// floor it cannot go under (the system prompt plus the question), and looping
	// would turn one clear failure into several slow ones.
	maxOverflowRescues = 2

	// overflowShrinkFactor is how much of the FAILED size to aim for when the provider
	// did not say what its limit is.
	//
	// Shrinking relative to what was actually rejected — rather than to the window we
	// believe in — is the point. The rejection is proof that our belief about the
	// window is wrong, so re-deriving a target from that same belief would produce
	// another prompt refused for the same reason.
	overflowShrinkFactor = 0.5

	// minStatedLimitTokens filters the integers scraped out of an error message. Real
	// context windows are far above this; smaller numbers in the text are HTTP status
	// codes and error codes, which must not be mistaken for a window.
	minStatedLimitTokens = 512

	// minQuestionTokens is the least room worth leaving for the final message. Below
	// this the prompt would carry instructions and no discernible request, which is a
	// worse outcome than telling the user it did not fit.
	minQuestionTokens = 32
)

// statedLimitPattern finds the integers in a provider's error text.
var statedLimitPattern = regexp.MustCompile(`\d{3,}`)

// overflowLimitFromError extracts the model's real context window from the rejection,
// or 0 when the provider did not state one.
//
// Worth doing because the provider usually DOES say, and it is the only authoritative
// number available — our own configured window is by definition wrong at this point.
// Guessing geometrically converges slowly when the configured value is far out, which
// it can be by an order of magnitude: a workspace configured for a 128k cloud model
// while a member runs an 8k local one is a 16x error, and halving from 16x takes four
// round trips to a model that refuses each one.
//
// Providers phrase it differently but consistently include both the limit and the
// requested size, with the LIMIT being the smaller of the two:
//
//	"This model's maximum context length is 8192 tokens, however you requested 10000"
//	"prompt is too long: 210000 tokens > 200000 maximum"
//
// So the smallest plausible integer in the message is the limit in both shapes. A
// wrong guess here is not dangerous: it only sets the next attempt's target, and the
// attempt is verified by the provider like any other.
func overflowLimitFromError(err error) int {
	if err == nil {
		return 0
	}
	best := 0
	for _, m := range statedLimitPattern.FindAllString(err.Error(), -1) {
		n, cerr := strconv.Atoi(m)
		if cerr != nil || n < minStatedLimitTokens {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

// rescueTarget is the outbound size to aim for on the next attempt, given the size that
// was just refused and what the provider said about why.
func rescueTarget(lim ModelLimits, refused []ChatMessage, err error, responseTokens int) int {
	sent := lim.EstimateMessagesTokens(refused)
	target := int(float64(sent) * overflowShrinkFactor)

	// Prefer the number the provider stated, leaving room for the response and framing.
	if stated := overflowLimitFromError(err); stated > 0 {
		room := stated - lim.ResponseReserve(responseTokens) - 256
		if room > 0 && room < target {
			target = room
		}
	}
	// Never aim above what we believe the model takes; the rejection means the truth is
	// at most that.
	if believed := lim.ConversationInputBudget(responseTokens); believed < target {
		target = believed
	}
	if target < 1 {
		target = 1
	}
	return target
}

// ShrinkToBudget returns a version of msgs whose estimated size fits budget under
// this model's tokenizer, plus whether anything was actually removed.
//
// What it preserves, in priority order:
//
//  1. Leading system messages, always and uncut. They are the instructions and the
//     safety rules; a request that obeys a truncated system prompt is worse than a
//     request that fails. If they alone exceed the budget this reports false rather
//     than sending something it knows is still too big.
//  2. The final message, which is the actual question. Clipped only as a last resort,
//     and with a visible marker so the model can say what it lost.
//  3. As much recent history as fits, dropped oldest-first.
//
// Turn boundaries come from PickCompactionBoundary, which already encodes the rule
// that a tool result may never head the message list — orphaning one from the
// assistant turn that requested it is itself a provider error, so a naive
// drop-the-oldest would trade an overflow for a 400.
func (l ModelLimits) ShrinkToBudget(msgs []ChatMessage, budget int) ([]ChatMessage, bool) {
	if len(msgs) == 0 || budget <= 0 {
		return msgs, false
	}
	if l.EstimateMessagesTokens(msgs) <= budget {
		return msgs, false
	}

	// Leading system messages are fixed cost.
	head := 0
	for head < len(msgs) && msgs[head].Role == roleSystem {
		head++
	}
	fixed := l.EstimateMessagesTokens(msgs[:head])
	if fixed >= budget {
		// Nothing this function may cut is large enough to help.
		return msgs, false
	}

	out := append([]ChatMessage{}, msgs[:head]...)
	rest := msgs[head:]

	if len(rest) > 0 {
		// Keep the newest whole turns that fit in what is left.
		if b := PickCompactionBoundary(msgs, head, budget-fixed); b > head && b < len(msgs) {
			out = append(out, msgs[b:]...)
		} else {
			// No usable turn boundary (a single enormous turn): keep the last message
			// only and let the clip below size it.
			out = append(out, rest[len(rest)-1])
		}
	}

	// Last resort: clip the final message so the total fits. Everything before it is
	// either uncuttable or already dropped.
	if l.EstimateMessagesTokens(out) > budget && len(out) > head {
		lastIdx := len(out) - 1
		others := l.EstimateMessagesTokens(out[:lastIdx])
		room := budget - others
		if room < minQuestionTokens {
			// No room left for the question itself; report honestly rather than
			// sending a prompt that asks nothing.
			return msgs, false
		}
		clipped := out[lastIdx]
		clipped.Content = l.TruncateToTokenBudget(clipped.Content, room)
		out[lastIdx] = clipped
	}

	if l.EstimateMessagesTokens(out) > budget {
		return msgs, false
	}
	return out, len(out) != len(msgs) || l.EstimateMessagesTokens(out) < l.EstimateMessagesTokens(msgs)
}

// ChatWithRescue is llm.Chat plus recovery from an over-long prompt.
//
// Use it instead of calling llm.Chat directly. It is a no-op on the happy path and on
// every other error, so the only behaviour it adds is turning a guaranteed-permanent
// failure into a shorter prompt that succeeds.
//
// Does NOT touch the circuit breaker or the token budget: callers own those, and an
// overflow is our own bookkeeping being wrong rather than the provider being
// unhealthy — recording it as a provider failure would trip a breaker guarding a model
// that is working fine.
func ChatWithRescue(ctx context.Context, llm LLMProvider, msgs []ChatMessage, opts ChatOptions) (string, error) {
	if llm == nil {
		return "", errServiceDisabled
	}
	out, err := llm.Chat(ctx, msgs, opts)
	if err == nil || !IsContextOverflow(err) {
		return out, err
	}

	lim := LimitsFrom(ctx)
	attempt := msgs
	for i := 0; i < maxOverflowRescues; i++ {
		target := rescueTarget(lim, attempt, err, opts.MaxTokens)
		shrunk, changed := lim.ShrinkToBudget(attempt, target)
		if !changed {
			logRescueExhausted(ctx, lim, attempt, err)
			break // cannot get smaller without discarding the request itself
		}
		logRescueAttempt(ctx, lim, attempt, shrunk, target, err)
		dropped := len(attempt) - len(shrunk)
		attempt = shrunk
		out, err = llm.Chat(ctx, attempt, opts)
		if err == nil {
			// Recorded only on SUCCESS: a rescue that also failed is reported to the
			// caller as an error, and telling someone their answer was shortened when
			// they did not get an answer would be nonsense.
			reportContextNotice(ctx, ContextNotice{Rescued: true, DroppedMessages: dropped})
			return out, nil
		}
		if !IsContextOverflow(err) {
			return out, err
		}
	}
	return out, err
}

// ChatStreamWithRescue is llm.ChatStream plus the same recovery.
//
// Safe to retry because a rejected prompt fails when the request is made, which is
// before the returned channel yields anything — so no partial answer has reached a
// user's screen and there is nothing to un-render. An overflow reported mid-stream
// (which would mean the provider accepted the prompt and changed its mind) is left
// alone for exactly that reason.
func ChatStreamWithRescue(ctx context.Context, llm LLMProvider, msgs []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	if llm == nil {
		return nil, errServiceDisabled
	}
	ch, err := llm.ChatStream(ctx, msgs, opts)
	if err == nil || !IsContextOverflow(err) {
		return ch, err
	}

	lim := LimitsFrom(ctx)
	attempt := msgs
	for i := 0; i < maxOverflowRescues; i++ {
		target := rescueTarget(lim, attempt, err, opts.MaxTokens)
		shrunk, changed := lim.ShrinkToBudget(attempt, target)
		if !changed {
			logRescueExhausted(ctx, lim, attempt, err)
			break
		}
		logRescueAttempt(ctx, lim, attempt, shrunk, target, err)
		dropped := len(attempt) - len(shrunk)
		attempt = shrunk
		ch, err = llm.ChatStream(ctx, attempt, opts)
		if err == nil {
			reportContextNotice(ctx, ContextNotice{Rescued: true, DroppedMessages: dropped})
			return ch, nil
		}
		if !IsContextOverflow(err) {
			return ch, err
		}
	}
	return ch, err
}

// --- observability -----------------------------------------------------------
//
// A rescue means the configured window is WRONG for the model that just answered — the
// provider refused a prompt our budget believed would fit. That is the single most
// actionable AI-configuration signal this system produces, and it was landing nowhere:
// sixteen call sites invoke the rescue without attaching a notice sink, and background
// jobs (code PRs, PR review, AI table columns) have no user-facing surface at all. An
// operator whose workspace silently shortens every long thread had no way to find out.
//
// So the log is the floor, independent of any UI. INFO rather than WARN for an attempt:
// the request is still going to succeed, and a line that reads like an incident for a
// recoverable event trains people to filter it. The exhausted case IS a user-visible
// failure and says so.

// logRescueAttempt records one shrink-and-retry with the numbers needed to act on it:
// which model, what we believed the budget was, what the provider said, and how far the
// prompt had to come down.
func logRescueAttempt(ctx context.Context, lim ModelLimits, refused, shrunk []ChatMessage, target int, err error) {
	stated := overflowLimitFromError(err)
	statedNote := "not stated"
	if stated > 0 {
		statedNote = "stated " + strconv.Itoa(stated)
	}
	helpers.LogInfoWithContext(ctx,
		"AI: prompt refused as too long by %s/%s (%s tokens); believed budget %s, retrying at %s "+
			"(was %s tokens over %d messages, now %s over %d). Raise the model's context window if this recurs. err=%s",
		lim.Provider, lim.Model, statedNote,
		strconv.Itoa(lim.ConversationInputBudget(0)), strconv.Itoa(target),
		strconv.Itoa(lim.EstimateMessagesTokens(refused)), len(refused),
		strconv.Itoa(lim.EstimateMessagesTokens(shrunk)), len(shrunk),
		trimForLog(err.Error()))
}

// logRescueExhausted records a prompt that could not be made to fit. This one IS the
// user's failure, so it is an error: the request is about to come back empty.
func logRescueExhausted(ctx context.Context, lim ModelLimits, attempt []ChatMessage, err error) {
	helpers.LogErrorWithContext(ctx,
		"AI: prompt could not be shortened enough for %s/%s — %s tokens over %d messages could not be reduced "+
			"below the model's limit, so the request fails. The system prompt plus the request is the floor. err=%s",
		lim.Provider, lim.Model, strconv.Itoa(lim.EstimateMessagesTokens(attempt)), len(attempt),
		trimForLog(err.Error()))
}

// overflowUserMessage is what a person sees when even the shrunk prompt did not fit.
// Says what to do about it, because "try again" is false here: the same request will
// fail the same way every time.
const overflowUserMessage = "That request was too long for the selected AI model's context window, even after trimming. " +
	"Try asking about a smaller selection or a shorter thread, or pick a model with a larger context window in the assistant's model menu."

// describeOverflow returns the user-facing text for a context-overflow error, or ""
// when err is not one. Kept next to the rescue so the message and the mechanism that
// tries to avoid needing it stay in the same file.
func describeOverflow(err error) string {
	if IsContextOverflow(err) {
		return overflowUserMessage
	}
	return ""
}

// trimForLog bounds an error string used in a log line.
func trimForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
