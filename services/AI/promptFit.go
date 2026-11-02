package ai

// Fitting a request to what the model, or its provider, accepts (V-157).
//
// The budgets in modelLimits.go size a prompt's parts from the context window an admin configured. A provider can
// refuse far less than that: Groq's on_demand tier refuses any one request above the model's tokens-per-minute limit
// (8,000 for openai/gpt-oss-20b, whose window is 131,072) with a 413, "Request too large ... Limit 8000, Requested
// 10306". The demo is configured with a 32,768 window, so on 31 Oct every ask that took the whole tool catalog was
// refused, and the person saw a 500.
//
// So a request is built from parts that give way in order (PromptParts.Fit): the instructions and the question never
// do; the tool section is held to a share of what is left, then old turns, then the workspace context take the rest.
// ChatFitted and ChatStreamFitted send it and, when the provider refuses it for size, fit it ONCE more to the smaller
// of the window and the limit the provider stated, and send that. The stated limit is remembered for the model for an
// hour, so the next request fits the first time. A request that still can't fit fails with ErrPromptTooLarge, which
// ClientError words for the person: never a 500.
//
// Both log what ChatWithRescue logs: a refit at INFO (the window set for the model is wrong for it), the limit learned
// once at INFO, and a request that can't be made to fit at ERROR, since that one is the person's failure.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// ErrPromptTooLarge is a request the model or its provider refused for size even after it was fitted to the limit
// the provider stated, or one whose instructions and question alone exceed it. The same request fails the same way
// every time, so the person is told to ask about less, not to try again.
var ErrPromptTooLarge = errors.New("ai: the request is too large for the model, even after fitting it")

const (
	// maxAskToolPromptTokens caps the assistant's tool section. The whole catalog is ~7,300 tokens, most of a
	// free-tier provider's per-request cap on its own.
	maxAskToolPromptTokens = 3000

	// fitMarginTokens is held back from every fitted request for the framing a provider adds to each message and
	// for the estimate's error: a request fitted to the token is refused by a tokenizer that counts a little higher.
	fitMarginTokens = 256

	// statedLimitHeadroom is the share of a provider's stated limit a refitted request aims for, for the same reason.
	statedLimitHeadroom = 0.95

	// statedLimitTTL is how long a stated limit holds. A provider's per-request limit moves with the plan (Groq's
	// Dev tier lifts the 8,000), so it is learned again rather than kept: at most one refused request per model per
	// hour per server.
	statedLimitTTL = time.Hour

	// limitSourceProvider: the window was lowered to a limit the provider stated when it refused a larger request.
	limitSourceProvider = "provider"
)

// PromptParts is a request made of parts that give way in order: the tool section, then old turns, then the
// workspace context. The instructions and the question never do.
type PromptParts struct {
	// System returns the instructions with a tool section of at most toolTokens (0: none).
	System func(toolTokens int) string
	// History is the conversation so far, oldest first.
	History []ChatMessage
	// Context is the workspace context, its most important sections first, so cutting its tail loses the least.
	Context string
	// Question is what was asked.
	Question string
}

// userMessage is the final message: the context, when there is any, then the question, last, so a cut context
// never takes the question with it.
func (p PromptParts) userMessage(context string) string {
	if context == "" {
		return p.Question
	}
	return fmt.Sprintf("Context from workspace:\n%s\n\nQuestion: %s", context, p.Question)
}

// floor is the least request p makes: the instructions without tools, and the question.
func (p PromptParts) floor() []ChatMessage {
	bare := ""
	if p.System != nil {
		bare = p.System(0)
	}
	return []ChatMessage{{Role: roleSystem, Content: bare}, {Role: roleUser, Content: p.userMessage("")}}
}

// Fit builds the request for lim, leaving room for a response of responseTokens. It reports false when the
// instructions without tools and the question alone exceed what lim accepts.
func (p PromptParts) Fit(ctx context.Context, lim ModelLimits, responseTokens int) ([]ChatMessage, bool) {
	input := lim.ContextWindow - lim.ResponseReserve(responseTokens) - fitMarginTokens
	least := p.floor()
	bare := least[0].Content
	floor := lim.EstimateMessagesTokens(least)
	if floor > input {
		return nil, false
	}
	room := input - floor
	system := bare
	// The tools take at most a third of what is left, so the context always keeps a share.
	if share := min(room/3, maxAskToolPromptTokens); share > 0 && p.System != nil {
		if s := p.System(share); s != bare {
			if extra := lim.EstimateTokens(s) - lim.EstimateTokens(bare); extra <= share {
				system, room = s, room-extra
			}
		}
	}
	history := lim.TrimHistoryForPrompt(ctx, p.History, min(lim.HistoryBudget(), room/2))
	room -= lim.EstimateMessagesTokens(history)
	context := ""
	if p.Context != "" && room > 0 {
		context = lim.TruncateForPrompt(ctx, p.Context, min(lim.ContextBudget(), room))
	}
	msgs := make([]ChatMessage, 0, len(history)+2)
	msgs = append(msgs, ChatMessage{Role: roleSystem, Content: system})
	msgs = append(msgs, history...)
	return append(msgs, ChatMessage{Role: roleUser, Content: p.userMessage(context)}), true
}

// ChatFitted sends the request parts make for the model's limits and, when the provider refuses it for size, fits it
// once more to the smaller of the window and the limit the provider stated and sends that. It returns the messages
// that were answered, for a caller that goes on with the conversation.
func ChatFitted(ctx context.Context, llm LLMProvider, opts ChatOptions, parts PromptParts) (string, []ChatMessage, error) {
	if llm == nil {
		return "", nil, errServiceDisabled
	}
	return sendFitted(ctx, opts, parts, func(msgs []ChatMessage) (string, error) { return llm.Chat(ctx, msgs, opts) })
}

// ChatStreamFitted is ChatFitted for a streamed answer. A refused request fails when it is made, before anything
// streams, so sending the refitted one is safe.
func ChatStreamFitted(ctx context.Context, llm LLMProvider, opts ChatOptions, parts PromptParts) (<-chan StreamChunk, []ChatMessage, error) {
	if llm == nil {
		return nil, nil, errServiceDisabled
	}
	return sendFitted(ctx, opts, parts, func(msgs []ChatMessage) (<-chan StreamChunk, error) { return llm.ChatStream(ctx, msgs, opts) })
}

// sendFitted is ChatFitted and ChatStreamFitted's one way through: fit parts to the model's limits and send them,
// and, when the provider refuses the request for size, fit it once more to what the provider stated and send that.
// Every way it can end is logged as ChatWithRescue logs its own.
func sendFitted[T any](ctx context.Context, opts ChatOptions, parts PromptParts, send func([]ChatMessage) (T, error)) (T, []ChatMessage, error) {
	var none T
	lim := LimitsFrom(ctx)
	msgs, ok := parts.Fit(ctx, lim, opts.MaxTokens)
	if !ok {
		logRescueExhausted(ctx, lim, parts.floor(), ErrPromptTooLarge)
		return none, nil, ErrPromptTooLarge
	}
	out, err := send(msgs)
	if err == nil || !IsContextOverflow(err) {
		return out, msgs, err
	}
	refit, target, ok := refitAfterRefusal(ctx, lim, msgs, err, parts, opts.MaxTokens)
	if !ok {
		logRescueExhausted(ctx, lim, msgs, err)
		return none, msgs, tooLarge(err)
	}
	logRescueAttempt(ctx, lim, msgs, refit, target, err)
	if out, err = send(refit); err != nil {
		if IsContextOverflow(err) {
			logRescueExhausted(ctx, lim, refit, err)
			return none, refit, tooLarge(err)
		}
		return none, refit, err
	}
	reportContextNotice(ctx, ContextNotice{Rescued: true})
	return out, refit, nil
}

// refitAfterRefusal fits parts to the smaller of lim's window and the limit the provider stated when it refused the
// request (half of what it refused when it states none), and remembers a stated limit for the model. It returns the
// window it fitted to, and reports false when no smaller request exists.
func refitAfterRefusal(ctx context.Context, lim ModelLimits, refused []ChatMessage, err error, parts PromptParts, responseTokens int) ([]ChatMessage, int, bool) {
	sent := lim.EstimateMessagesTokens(refused)
	limit := 0
	if stated := overflowLimitFromError(err); stated > 0 && stated < sent+lim.ResponseReserve(responseTokens) {
		limit = statedWindow(stated)
		rememberStatedLimit(ctx, lim, stated)
	} else {
		// Nothing usable stated: the rejection proves our belief about the window wrong, so aim below what was sent.
		limit = int(float64(sent+lim.ResponseReserve(responseTokens)) * overflowShrinkFactor)
	}
	refitLim := lim
	if limit < refitLim.ContextWindow {
		refitLim.ContextWindow, refitLim.Source = limit, limitSourceProvider
	}
	msgs, ok := parts.Fit(ctx, refitLim, responseTokens)
	if !ok || lim.EstimateMessagesTokens(msgs) >= sent {
		return nil, refitLim.ContextWindow, false
	}
	return msgs, refitLim.ContextWindow, true
}

// tooLarge is ErrPromptTooLarge carrying the provider's refusal, for the server's log.
func tooLarge(err error) error { return fmt.Errorf("%w (%w)", ErrPromptTooLarge, err) }

// --- stated limits -----------------------------------------------------------------

type statedLimit struct {
	tokens int
	until  time.Time
}

// statedLimits remembers, per model, the request size a provider said it accepts when it refused a larger one.
var statedLimits = struct {
	sync.Mutex
	m map[string]statedLimit
}{m: map[string]statedLimit{}}

func statedLimitKey(l ModelLimits) string {
	return strings.ToLower(strings.TrimSpace(l.Provider)) + "\x00" + strings.ToLower(strings.TrimSpace(l.Model))
}

// statedWindow is the window a limit a provider stated gives: the limit less its headroom, so a tokenizer that counts
// a little higher than our estimate still takes the request.
func statedWindow(stated int) int { return int(float64(stated) * statedLimitHeadroom) }

// rememberStatedLimit records the window a stated limit gives as the most a request to lim's model may be, and logs
// it once. A window below any real one is a misread (an error code), and one at or above lim's changes nothing.
func rememberStatedLimit(ctx context.Context, lim ModelLimits, stated int) {
	tokens := statedWindow(stated)
	if tokens < minContextWindow || tokens >= lim.ContextWindow || (lim.Provider == "" && lim.Model == "") {
		return
	}
	key := statedLimitKey(lim)
	now := time.Now()
	statedLimits.Lock()
	held, ok := statedLimits.m[key]
	statedLimits.m[key] = statedLimit{tokens: tokens, until: now.Add(statedLimitTTL)}
	statedLimits.Unlock()
	// Requests refused together each learn the same limit: one line says so.
	if ok && held.tokens == tokens && now.Before(held.until) {
		return
	}
	helpers.LogInfoWithContext(ctx,
		"AI: %s/%s stated a limit of %d tokens a request when it refused a larger one, below the context window of %d "+
			"set for it, so its requests are fitted to %d tokens for the next hour. A window set to what the provider "+
			"takes fits them the first time.",
		lim.Provider, lim.Model, stated, lim.ContextWindow, tokens)
}

// withinStatedLimit returns l with its window lowered to the limit its provider stated, while that holds.
func (l ModelLimits) withinStatedLimit() ModelLimits {
	if l.Provider == "" && l.Model == "" {
		return l
	}
	key := statedLimitKey(l)
	statedLimits.Lock()
	s, ok := statedLimits.m[key]
	if ok && time.Now().After(s.until) {
		delete(statedLimits.m, key)
		ok = false
	}
	statedLimits.Unlock()
	if ok && s.tokens < l.ContextWindow {
		l.ContextWindow, l.Source = s.tokens, limitSourceProvider
	}
	return l
}
