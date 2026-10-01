package ai

// Model routing: which allowlisted model each kind of background work runs on
// (migration 172).
//
// It covers only work nobody chose a model for. A person's chat uses their own
// pick, an agent uses its own model, and a channel with a pinned model keeps
// it: those are decisions someone made, and routing never overrides them.
//
// Resolution reuses ResolveExplicitModel, so a routed model gets the same
// guarantees as an agent's: it must build, it gets its own circuit breaker,
// and under local-only mode a cloud model is refused (with an audit row) in
// favour of the local default.

import (
	"context"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// Purposes of background work that can be routed.
const (
	// PurposeSummaries is catch-ups, channel summaries, briefings, team
	// reports, nudges and import digests: short, frequent, high volume.
	PurposeSummaries = "summaries"
	// PurposeMeetings is meeting recaps: long transcripts, worth a model with
	// a large context window.
	PurposeMeetings = "meetings"
	// PurposeMemory is extracting decisions and facts into workspace memory:
	// structured output, run on many messages.
	PurposeMemory = "memory"
)

// Purpose describes one routable kind of work for the admin screen.
type Purpose struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Purposes lists every routable purpose, in the order the admin sees them.
var Purposes = []Purpose{
	{PurposeSummaries, "Summaries and briefings", "Catch-ups, channel summaries, morning briefings, team reports and nudges. Frequent and short, so a fast model is usually the right choice."},
	{PurposeMeetings, "Meeting recaps", "Recaps of call transcripts. Transcripts are long, so pick a model with a large context window."},
	{PurposeMemory, "Workspace memory", "Pulling decisions, owners and dates out of conversations. Runs on many messages."},
}

// IsPurpose reports whether key names a routable purpose.
func IsPurpose(key string) bool {
	for _, p := range Purposes {
		if p.Key == key {
			return true
		}
	}
	return false
}

// RouteFor returns the configured target for a purpose, if any.
func (c *AIConfig) RouteFor(purpose string) (aiModels.RouteTarget, bool) {
	if c == nil || c.Routing == nil {
		return aiModels.RouteTarget{}, false
	}
	t, ok := c.Routing[purpose]
	return t, ok && t.Model != ""
}

// ResolvePurpose returns the client and breaker for one kind of background
// work: the routed model when one is set and usable, otherwise the workspace
// default.
func (s *AIService) ResolvePurpose(ctx context.Context, purpose string) (LLMProvider, *CircuitBreaker) {
	if s == nil || !s.IsEnabled() {
		return s.llmOrNil(), nil
	}
	if t, ok := s.Config.RouteFor(purpose); ok {
		llm, cb, _ := s.ResolveExplicitModel(ctx, t.ProviderID, t.Model)
		return llm, cb
	}
	var cb *CircuitBreaker
	if s.Resiliency != nil {
		cb = s.Resiliency.CB
	}
	return s.LLM, cb
}

// SummarizeFor is Summarize on the model routed for purpose.
func (s *AIService) SummarizeFor(ctx context.Context, purpose, content, systemPrompt string) (string, error) {
	if !s.IsEnabled() {
		return s.SummarizeWith(ctx, nil, content, systemPrompt)
	}
	llm, cb := s.ResolvePurpose(ctx, purpose)
	out, err := s.SummarizeWith(ctx, llm, content, systemPrompt)
	if cb != nil {
		cb.RecordResult(err)
	}
	return out, err
}
