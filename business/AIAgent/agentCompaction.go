package business

// Runner-side glue for conversation compaction.
//
// A tool-loop conversation only grows: each step appends the assistant turn plus
// one observation per tool call. Before this, a long run walked into the model's
// context window and either lost its own system prompt (Ollama truncates from
// the front) or died with "the AI model call failed" on a provider 400 —
// throwing away every completed step. Clipping a single observation
// (truncateObservation) bounds ONE result, never the total.
//
// The generic mechanics live in services/AI (ai.CompactConversation): boundary
// selection that can never orphan a tool result, an LLM summary of the folded
// turns, mechanically extracted working state, and the human's own messages
// preserved verbatim. This file supplies only what is specific to the agent
// loop: which `user` turns are scaffolding rather than a person talking, and the
// audit/logging around a compaction.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
	"strings"
)

const (
	// toolResultsPrefix opens the synthetic `user` turn the text protocol uses
	// to feed observations back into the loop. Named so compaction can tell it
	// apart from a real human message.
	toolResultsPrefix = "Tool results:"

	// maxRunCompactions bounds how many times ONE run may compact. A run that
	// keeps overflowing after this many folds is not going to finish usefully;
	// the ordinary step/token bounds then end it with its partial result.
	maxRunCompactions = 6

	// maxContextRescues bounds the reactive compactions triggered by a provider
	// rejecting an oversized prompt. A rescue does NOT consume a step (the same
	// step is retried on the smaller conversation), so it must be bounded
	// independently.
	maxContextRescues = 2
)

// compactionNote is the transcript marker recorded when the conversation was
// compacted before a step: it makes an otherwise invisible mechanism auditable
// ("older turns were folded here, and this is how much it saved") and lets the
// run-history UI show a divider instead of an unexplained memory gap.
type compactionNote struct {
	// Folded is the cumulative number of messages replaced by the summary.
	Folded int `json:"folded"`
	// Round is which compaction this was within the run (1-based).
	Round int `json:"round"`
	// TokensBefore / TokensAfter are the estimated prompt size either side.
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	// Summarized is false when the summarizer was unavailable and only
	// mechanical working state was kept.
	Summarized bool `json:"summarized"`
	// Rescue is true when a provider had already rejected the prompt as too
	// large and this compaction recovered the run instead of failing it.
	Rescue bool `json:"rescue,omitempty"`
}

// isLoopScaffold reports whether a `user` message was injected by the runner
// (an observation turn, a corrective nudge, a previous compaction block) rather
// than written by a person. Scaffolding is summarized like any other turn, but
// it must never be preserved in the block's verbatim "what the human said"
// list — otherwise the agent's own machinery drowns out the actual request.
func isLoopScaffold(m ai.ChatMessage) bool {
	if m.Role != "user" {
		return false
	}
	c := strings.TrimSpace(m.Content)
	switch {
	case c == "":
		return true
	case strings.HasPrefix(c, toolResultsPrefix):
		return true
	case c == stallCorrection, c == deflectionCorrection:
		return true
	case strings.HasPrefix(c, claimCorrectionPrefix):
		return true
	case ai.IsCompactedBlock(m):
		return true
	}
	return false
}

// compactRunConversation folds the older turns of a live run's conversation and
// returns the conversation to send from now on. It is safe to call at any point
// in the loop: when there is nothing worth folding (or folding wouldn't shrink
// the prompt) it reports Compacted=false and the caller keeps what it had.
//
// rescue marks a compaction triggered by a provider rejecting an oversized
// prompt (rather than the proactive threshold), which is recorded in the
// transcript so an operator can see the run was saved rather than lucky.
func compactRunConversation(
	ctx context.Context,
	llm ai.LLMProvider,
	msgs []ai.ChatMessage,
	prior *ai.CompactionState,
	responseTokens int,
	modelLabel string,
	agentID uuid.UUID,
	rescue bool,
) (ai.CompactResult, *compactionNote) {
	res := ai.CompactConversation(ctx, llm, msgs, ai.CompactOptions{
		Prior:          prior,
		ResponseTokens: responseTokens,
		SyntheticUser:  isLoopScaffold,
		Model:          modelLabel,
	})
	if !res.Compacted || res.State == nil {
		return res, nil
	}
	if res.SummaryErr != nil {
		// Not a failure: the block still carries the mechanically extracted
		// working state and the human's messages. Logged because a persistently
		// failing summarizer degrades long-run quality.
		helpers.LogErrorWithContext(ctx, "agentRunner: compaction summary unavailable (agent=%s), kept mechanical state only: %v", agentID, res.SummaryErr)
	}
	helpers.LogInfoWithContext(ctx, "agentRunner: compacted context (agent=%s round=%d folded=%d tokens %d->%d rescue=%t)",
		agentID, res.State.Rounds, res.State.Folded, res.State.TokensBefore, res.State.TokensAfter, rescue)
	return res, &compactionNote{
		Folded:       res.State.Folded,
		Round:        res.State.Rounds,
		TokensBefore: res.State.TokensBefore,
		TokensAfter:  res.State.TokensAfter,
		Summarized:   !res.State.Trimmed,
		Rescue:       rescue,
	}
}
