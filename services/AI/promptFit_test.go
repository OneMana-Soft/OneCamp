package ai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// V-157: on 31 Oct the demo's AI ask answered 500 for every question about a board. Groq's on_demand tier refuses
// any one request above the model's tokens-per-minute limit (8,000 for openai/gpt-oss-20b) with a 413, and a question
// that named no tool group was sent the whole 58-tool catalog (~7,300 tokens) on top of the workspace context. The
// refusal was not recognised as one for size, the breaker counted it as an outage, and the controller echoed Groq's
// text (its organization id included) to the browser.

// groq413 and groq429 are Groq's bodies, verbatim but for the organization id.
const groq413 = "{\"error\":{\"message\":\"Request too large for model `openai/gpt-oss-20b` in organization `org_01secretorg` service tier `on_demand` on tokens per minute (TPM): Limit 8000, Requested 10306, please reduce your message size and try again. Need more tokens? Upgrade to Dev Tier today at https://console.groq.com/settings/billing\",\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\"}}"
const groq429 = "{\"error\":{\"message\":\"Rate limit reached for model `openai/gpt-oss-20b` in organization `org_01secretorg` service tier `on_demand` on tokens per minute (TPM): Limit 8000, Used 7000, Requested 10429. Please try again in 7.5s.\",\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\"}}"

func TestAProviderRefusalForSizeIsAnOverflowAndStatesItsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, groq413)
	}))
	defer srv.Close()
	p := NewOpenAICompatibleProviderWithOpts("k", "openai/gpt-oss-20b", "", srv.URL, 0, ProviderOpenAICompatible, ProviderOptions{})
	_, err := p.Chat(context.Background(), []ChatMessage{{Role: roleUser, Content: "hi"}}, ChatOptions{MaxTokens: 16})
	if err == nil || !IsContextOverflow(err) {
		t.Fatalf("Groq's 413 is a refusal for size, got %v", err)
	}
	if errors.Is(err, ErrProviderRateLimited) {
		t.Fatal("a refusal for size is not a rate limit: waiting won't make the same request fit")
	}
	if got := overflowLimitFromError(err); got != 8000 {
		t.Fatalf("the stated limit is 8000, read %d", got)
	}
	status, words := ClientError(context.Background(), err)
	if status != http.StatusUnprocessableEntity || words != overflowUserMessage {
		t.Fatalf("a request too large is answered 422 with the overflow words, got %d %q", status, words)
	}
	// A rate limit whose numbers hold "413"-free text but "429" by chance stays a rate limit, not an overflow.
	rl := providerStatusError("openai: API returned", http.StatusTooManyRequests, []byte(groq429))
	if IsContextOverflow(rl) || !errors.Is(rl, ErrProviderRateLimited) {
		t.Fatalf("Groq's 429 is a rate limit only: overflow=%v", IsContextOverflow(rl))
	}
	if status, _ := ClientError(context.Background(), rl); status != http.StatusTooManyRequests {
		t.Fatalf("a rate limit is answered 429, got %d", status)
	}
}

func TestNoClientErrorCarriesTheProvidersText(t *testing.T) {
	for _, err := range []error{
		providerStatusError("openai: API returned", http.StatusRequestEntityTooLarge, []byte(groq413)),
		providerStatusError("openai: API returned", http.StatusTooManyRequests, []byte(groq429)),
		providerStatusError("openai: API returned", http.StatusBadRequest, []byte(`{"error":{"message":"org_01secretorg rejected: model foo"}}`)),
		errors.New(`Post "http://10.0.0.7:11434/api/chat": dial tcp 10.0.0.7:11434: connect: connection refused`),
	} {
		status, words := ClientError(context.Background(), err)
		if status < 400 || words == "" || strings.Contains(words, "org_") || strings.Contains(words, "10.0.0.7") || strings.Contains(words, "Request too large") {
			t.Fatalf("ClientError(%v) = %d %q: the provider's own text reached the person", err, status, words)
		}
	}
}

func TestARefusalForSizeLeavesTheBreakerClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	refusal := providerStatusError("openai: API returned", http.StatusRequestEntityTooLarge, []byte(groq413))
	for i := 0; i < 10; i++ {
		cb.RecordResult(refusal)
	}
	if err := cb.Allow(); err != nil {
		t.Fatalf("ten requests refused for size opened the breaker, so the next ask about anything is refused: %v", err)
	}
}

func TestABoardQuestionGetsTheSearchToolsNotTheCatalog(t *testing.T) {
	lim := ModelLimits{ContextWindow: 32768}
	full := lim.EstimateTokens(buildToolPrompt(ToolRegistry))
	board := BuildToolPromptWithin(lim, "What does the Launch retro board say about rollback steps?", maxAskToolPromptTokens)
	if !strings.Contains(board, "search_workspace") || strings.Contains(board, "gmail_send") {
		t.Fatalf("a board question reaches search and nothing unrelated:\n%s", board)
	}
	for _, q := range []string{"What does the Launch retro board say about rollback steps?", "What did Acme say about pricing?", "xyzzy plugh"} {
		got := lim.EstimateTokens(BuildToolPromptWithin(lim, q, maxAskToolPromptTokens))
		if got > maxAskToolPromptTokens || got >= full {
			t.Fatalf("%q got %d tokens of tools (cap %d, catalog %d)", q, got, maxAskToolPromptTokens, full)
		}
	}
	// The board group's own tools: the search, and what the board is linked to.
	if got := toolNames(selectToolsForQuery("What does the Launch retro board say about rollback steps?")); strings.Join(got, " ") != "search_workspace related_items how_connected" {
		t.Fatalf("a board question is routed to the search and the connection tools, got %v", got)
	}
	for _, name := range []string{"related_items", "how_connected"} {
		if !strings.Contains(board, "- "+name+":") {
			t.Fatalf("a board question reaches what the board is linked to (%s):\n%s", name, board)
		}
	}
	// A question that names its tools gets the same ones as before.
	task := "Which tasks are due this week, and who owns them?"
	if got, want := BuildToolPromptWithin(lim, task, maxAskToolPromptTokens), buildToolPrompt(selectToolsForQuery(task)); got != want {
		t.Fatalf("a task question's tools changed")
	}
	// A budget too small for one tool drops the section rather than overflowing.
	if got := BuildToolPromptWithin(lim, task, 50); got != "" {
		t.Fatalf("a 50-token budget still got a tool section of %d tokens", lim.EstimateTokens(got))
	}
}

// groqLike refuses, the way Groq does, any request whose estimate plus its response exceeds limit. Its refusal
// states limit, or states when set: a provider that takes less than it says.
type groqLike struct {
	limit    int
	states   int
	lim      ModelLimits
	attempts [][]ChatMessage
}

func (p *groqLike) refuse(msgs []ChatMessage, opts ChatOptions) error {
	p.attempts = append(p.attempts, msgs)
	if p.lim.EstimateMessagesTokens(msgs)+opts.MaxTokens > p.limit {
		stated := p.limit
		if p.states > 0 {
			stated = p.states
		}
		return providerStatusError("openai: API returned", http.StatusRequestEntityTooLarge, []byte(strings.Replace(groq413, "Limit 8000", "Limit "+strconv.Itoa(stated), 1)))
	}
	return nil
}

func (p *groqLike) Chat(_ context.Context, msgs []ChatMessage, opts ChatOptions) (string, error) {
	if err := p.refuse(msgs, opts); err != nil {
		return "", err
	}
	return "answered", nil
}

func (p *groqLike) ChatStream(_ context.Context, msgs []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	if err := p.refuse(msgs, opts); err != nil {
		return nil, err
	}
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "answered", Done: true}
	close(ch)
	return ch, nil
}

func (p *groqLike) ProviderName() ProviderType { return ProviderOpenAICompatible }

func askParts(contextChars int) PromptParts {
	return PromptParts{
		System: func(toolTokens int) string {
			return "You answer from the workspace." + BuildToolPromptWithin(ModelLimits{ContextWindow: 32768}, "What does the board say?", toolTokens)
		},
		Context:  strings.Repeat("A note on the launch board about the rollback steps. ", contextChars/53),
		Question: "What does the Launch retro board say about rollback steps?",
	}
}

func forgetStatedLimitsForTest(t *testing.T) {
	t.Helper()
	reset := func() { statedLimits.Lock(); statedLimits.m = map[string]statedLimit{}; statedLimits.Unlock() }
	reset()
	t.Cleanup(reset)
}

func TestChatFittedRefitsOnceToTheStatedLimitAndRemembersIt(t *testing.T) {
	forgetStatedLimitsForTest(t)
	lim := ModelLimits{Provider: "openai_compatible", Model: "openai/gpt-oss-20b", ContextWindow: 32768}
	ctx := WithModelLimits(context.Background(), lim)
	p := &groqLike{limit: 8000, lim: lim}
	opts := ChatOptions{MaxTokens: 1024}
	out, sent, err := ChatFitted(ctx, p, opts, askParts(60000))
	if err != nil || out != "answered" {
		t.Fatalf("a request fitted to the stated limit is answered: %q %v", out, err)
	}
	if len(p.attempts) != 2 {
		t.Fatalf("one refusal, one refit: %d attempts", len(p.attempts))
	}
	if first, second := lim.EstimateMessagesTokens(p.attempts[0]), lim.EstimateMessagesTokens(sent); second >= first || second+opts.MaxTokens > 8000 {
		t.Fatalf("the retry must be smaller and within 8000: %d then %d", first, second)
	}
	if q := sent[len(sent)-1].Content; !strings.HasSuffix(q, "Question: What does the Launch retro board say about rollback steps?") {
		t.Fatalf("the question was cut: %q", q[max(0, len(q)-80):])
	}
	// The next request to the same model fits the first time: the window the refit was fitted to, the stated limit
	// less its headroom, is the one remembered (the raw limit is refused again by a tokenizer that counts higher).
	if got, want := LimitsFrom(ctx).ContextWindow, int(8000*statedLimitHeadroom); got != want {
		t.Fatalf("the stated limit less its headroom is remembered for the model: window %d, want %d", got, want)
	}
	p.attempts = nil
	if _, _, err := ChatFitted(ctx, p, opts, askParts(60000)); err != nil || len(p.attempts) != 1 {
		t.Fatalf("the remembered limit makes the next request fit at once: %v after %d attempts", err, len(p.attempts))
	}
	// Another model keeps its own window.
	if got := LimitsFrom(WithModelLimits(context.Background(), ModelLimits{Provider: "anthropic", Model: "c", ContextWindow: 32768})).ContextWindow; got != 32768 {
		t.Fatalf("one model's stated limit reached another model: %d", got)
	}
}

func TestChatStreamFittedRefitsOnce(t *testing.T) {
	forgetStatedLimitsForTest(t)
	lim := ModelLimits{Provider: "openai_compatible", Model: "s", ContextWindow: 32768}
	p := &groqLike{limit: 8000, lim: lim}
	ch, sent, err := ChatStreamFitted(WithModelLimits(context.Background(), lim), p, ChatOptions{MaxTokens: 1024}, askParts(60000))
	if err != nil || ch == nil || len(p.attempts) != 2 || lim.EstimateMessagesTokens(sent)+1024 > 8000 {
		t.Fatalf("the stream is refitted once: err %v, %d attempts", err, len(p.attempts))
	}
}

func TestChatFittedNamesARequestThatCannotFit(t *testing.T) {
	forgetStatedLimitsForTest(t)
	lim := ModelLimits{Provider: "openai_compatible", Model: "tiny", ContextWindow: 32768}
	p := &groqLike{limit: 1100, lim: lim}
	_, _, err := ChatFitted(WithModelLimits(context.Background(), lim), p, ChatOptions{MaxTokens: 1024}, askParts(60000))
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("a request that can't fit is ErrPromptTooLarge, got %v", err)
	}
	if len(p.attempts) > 2 {
		t.Fatalf("at most one retry: %d attempts", len(p.attempts))
	}
	if status, words := ClientError(context.Background(), err); status != http.StatusUnprocessableEntity || strings.Contains(words, "org_") {
		t.Fatalf("answered %d %q", status, words)
	}
}

// fittedAsks are ChatFitted and ChatStreamFitted, for what holds for both.
var fittedAsks = []struct {
	name string
	ask  func(context.Context, LLMProvider, ChatOptions, PromptParts) error
}{
	{"plain", func(ctx context.Context, llm LLMProvider, opts ChatOptions, parts PromptParts) error {
		_, _, err := ChatFitted(ctx, llm, opts, parts)
		return err
	}},
	{"streamed", func(ctx context.Context, llm LLMProvider, opts ChatOptions, parts PromptParts) error {
		_, _, err := ChatStreamFitted(ctx, llm, opts, parts)
		return err
	}},
}

// serverLog is the server's log while a test runs: captureLog points helpers.Logger at it.
type serverLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

// lines returns the lines logged at level that hold every one of words.
func (l *serverLog) lines(level string, words ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.text.String(), "\n") {
		if !strings.Contains(line, " level="+level+" ") {
			continue
		}
		all := true
		for _, w := range words {
			all = all && strings.Contains(line, w)
		}
		if all {
			out = append(out, line)
		}
	}
	return out
}

func captureLog(t *testing.T) *serverLog {
	t.Helper()
	l := &serverLog{}
	prev := helpers.Logger
	helpers.Logger = slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { helpers.Logger = prev })
	return l
}

// A refit means the window set for the model is wrong for it, the most useful thing the AI's settings can be told,
// so both asks log it as ChatWithRescue does: the refusal and the refit at INFO, and the limit learned, once.
func TestAFittedAskLogsItsRefitAndTheLimitItLearned(t *testing.T) {
	for _, f := range fittedAsks {
		t.Run(f.name, func(t *testing.T) {
			forgetStatedLimitsForTest(t)
			log := captureLog(t)
			lim := ModelLimits{Provider: "openai_compatible", Model: "openai/gpt-oss-20b", ContextWindow: 32768}
			ctx := WithModelLimits(context.Background(), lim)
			p := &groqLike{limit: 8000, lim: lim}
			if err := f.ask(ctx, p, ChatOptions{MaxTokens: 1024}, askParts(60000)); err != nil || len(p.attempts) != 2 {
				t.Fatalf("refused once, then answered: %v after %d attempts", err, len(p.attempts))
			}
			if got := log.lines("INFO", "prompt refused as too long by openai_compatible/openai/gpt-oss-20b (stated 8000 tokens)"); len(got) != 1 {
				t.Fatalf("the refusal and the refit are logged once at INFO, with the model and the limit it stated: %q", got)
			}
			learned := strconv.Itoa(int(8000 * statedLimitHeadroom))
			if got := log.lines("INFO", "openai_compatible/openai/gpt-oss-20b stated a limit of 8000 tokens", "fitted to "+learned+" tokens"); len(got) != 1 {
				t.Fatalf("the limit learned for the model is logged once at INFO: %q", got)
			}
			if got := log.lines("ERROR"); len(got) != 0 {
				t.Fatalf("an ask that was answered logs no error: %q", got)
			}
			// The next ask fits the learned limit at once, and has nothing to log.
			p.attempts = nil
			if err := f.ask(ctx, p, ChatOptions{MaxTokens: 1024}, askParts(60000)); err != nil || len(p.attempts) != 1 {
				t.Fatalf("the learned limit fits the next ask at once: %v after %d attempts", err, len(p.attempts))
			}
			if got := log.lines("INFO", "openai_compatible/openai/gpt-oss-20b"); len(got) != 2 {
				t.Fatalf("an ask that fits logs nothing more: %q", got)
			}
		})
	}
}

// A request that can't be made to fit is the person's failure, so it is logged at ERROR however it ends: before
// anything is sent, when no smaller request exists, or when the refit is refused too.
func TestAFittedAskThatCannotFitLogsAnError(t *testing.T) {
	cases := []struct {
		name          string
		window, limit int
		states        int
	}{
		{"its instructions and question alone are over the window", 1200, 1 << 30, 0},
		{"no smaller request fits the limit it states", 32768, 1100, 0},
		{"the refit is refused too", 32768, 4000, 8000},
	}
	for _, f := range fittedAsks {
		for _, c := range cases {
			t.Run(f.name+"/"+c.name, func(t *testing.T) {
				forgetStatedLimitsForTest(t)
				log := captureLog(t)
				lim := ModelLimits{Provider: "openai_compatible", Model: "small", ContextWindow: c.window}
				p := &groqLike{limit: c.limit, states: c.states, lim: lim}
				err := f.ask(WithModelLimits(context.Background(), lim), p, ChatOptions{MaxTokens: 1024}, askParts(60000))
				if !errors.Is(err, ErrPromptTooLarge) {
					t.Fatalf("a request that can't fit is ErrPromptTooLarge, got %v", err)
				}
				if got := log.lines("ERROR", "could not be shortened enough for openai_compatible/small"); len(got) != 1 {
					t.Fatalf("a request that can't be made to fit is logged once at ERROR: %q", got)
				}
			})
		}
	}
}

// Fit holds the tool section to its share whatever the section's builder does with the budget it is given.
func TestFitLeavesOutAToolSectionOverItsShare(t *testing.T) {
	const bare = "You answer from the workspace."
	parts := PromptParts{
		System: func(toolTokens int) string {
			if toolTokens == 0 {
				return bare
			}
			return bare + strings.Repeat(" a tool that ignores its budget", 4000)
		},
		Question: "What does the board say?",
	}
	lim := ModelLimits{ContextWindow: 32768}
	msgs, ok := parts.Fit(context.Background(), lim, 1024)
	if !ok || msgs[0].Content != bare {
		t.Fatalf("a tool section over its share is left out: ok %v, %d tokens of instructions", ok, lim.EstimateTokens(msgs[0].Content))
	}
}

// Requests refused together each learn the provider's limit: the log says so once, and again only when it changes.
func TestALimitLearnedTwiceIsLoggedOnce(t *testing.T) {
	forgetStatedLimitsForTest(t)
	log := captureLog(t)
	lim := ModelLimits{Provider: "openai_compatible", Model: "m", ContextWindow: 32768}
	for _, stated := range []int{8000, 8000, 6000} {
		rememberStatedLimit(context.Background(), lim, stated)
	}
	if got := log.lines("INFO", "openai_compatible/m stated a limit of"); len(got) != 2 {
		t.Fatalf("a limit learned twice is logged once, and a new one again: %q", got)
	}
	if got := LimitsFrom(WithModelLimits(context.Background(), lim)).ContextWindow; got != int(6000*statedLimitHeadroom) {
		t.Fatalf("the newest limit holds: window %d", got)
	}
}

// A provider's per-request limit moves with its plan, so a stated limit holds for its hour and is then learned again.
func TestAStatedLimitLapsesAfterItsHour(t *testing.T) {
	forgetStatedLimitsForTest(t)
	lim := ModelLimits{Provider: "openai_compatible", Model: "m", ContextWindow: 32768}
	hold := func(until time.Time) {
		statedLimits.Lock()
		statedLimits.m[statedLimitKey(lim)] = statedLimit{tokens: 7600, until: until}
		statedLimits.Unlock()
	}
	hold(time.Now().Add(time.Minute))
	if got := lim.withinStatedLimit().ContextWindow; got != 7600 {
		t.Fatalf("a stated limit within its hour lowers the window: %d", got)
	}
	hold(time.Now().Add(-time.Second))
	if got := lim.withinStatedLimit().ContextWindow; got != 32768 {
		t.Fatalf("a stated limit past its hour still lowers the window: %d", got)
	}
	statedLimits.Lock()
	_, kept := statedLimits.m[statedLimitKey(lim)]
	statedLimits.Unlock()
	if kept {
		t.Fatal("a lapsed stated limit is forgotten")
	}
}

// An overflow is answered 422 with words a person can act on, whatever numbers its text holds and however it was
// recognised: by type (a 413, or a request that could not be fitted before anything was sent) or by its words.
func TestEveryOverflowGetsTheOverflowWords(t *testing.T) {
	ctx := context.Background()
	for name, err := range map[string]error{
		// Groq's 413 for a request of 10,429 tokens: "429" among its numbers is not a rate limit.
		"a 413 whose numbers hold 429":           providerStatusError("openai: API returned", http.StatusRequestEntityTooLarge, []byte(strings.Replace(groq413, "Requested 10306", "Requested 10429", 1))),
		"a request too large before it was sent": ErrPromptTooLarge,
		// OpenAI's overflow is a 400, known only by its words.
		"an overflow known by its words, its numbers holding 429": providerStatusError("openai: API returned", http.StatusBadRequest, []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 10429 tokens. Please reduce the length of the messages.","code":"context_length_exceeded"}}`)),
	} {
		if words := FriendlyProviderError(err); words != overflowUserMessage {
			t.Errorf("%s: FriendlyProviderError gave %q", name, words)
		}
		if status, words := ClientError(ctx, err); status != http.StatusUnprocessableEntity || words != overflowUserMessage {
			t.Errorf("%s: answered %d %q, want 422 with the overflow words", name, status, words)
		}
	}
}
