package business

import (
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// failedWriteTools filters a run's errored-tool list down to WRITE (non
// read-only) tools — the only failures that make a "done" claim dangerous. An
// unknown/MCP tool is treated as a write by ToolIsReadOnly (fail-safe), so an
// external action that errored always counts. Read failures are excluded: a
// failed search doesn't make a subsequent answer a false success.
func failedWriteTools(failedTools []string) []string {
	out := make([]string, 0, len(failedTools))
	for _, t := range failedTools {
		if !ai.ToolIsReadOnly(t) {
			out = append(out, t)
		}
	}
	return out
}

// Deterministic post-run claim verification — the "verifier loop" lesson from
// large-scale background coding agents (Spotify's Honk, Anthropic's guidance):
// the most damaging failure is a run that claims success while a WRITE it
// relied on actually FAILED — a confident but false "done" that erodes trust.
//
// Unlike the OPTIONAL, model-driven self-critique in agentVerify.go (one extra
// LLM call, off by default), this gate is PURE, deterministic, and ALWAYS ON:
// it compares the draft's own success language against the run's GROUND-TRUTH
// signal (which write tools errored), so it costs no model call and cannot be
// fooled by the model's prose. When the draft asserts completion but a write
// tool errored — and the draft does not already own that failure — the runner
// forces ONE honest-rewrite turn instead of shipping the false success. It is
// model-agnostic: it inspects text + execution facts, never a provider field.

const (
	// maxClaimRetries bounds deterministic honesty corrections per run. One
	// firm nudge is enough; beyond that the runner falls back to appending the
	// honest failed-tools disclosure rather than looping.
	maxClaimRetries = 1
)

// successAssertionPhrases are lowercase markers that a draft is asserting the
// work is DONE. Kept to unambiguous completion language so a hedged or
// partial-progress answer is not flagged.
var successAssertionPhrases = []string{
	"done", "all set", "completed", "i've created", "i have created", "i created",
	"i've updated", "i have updated", "i updated", "i've posted", "i posted",
	"i've sent", "i have sent", "i sent", "i've added", "i added",
	"successfully", "has been created", "has been updated", "has been posted",
	"have been created", "task created", "created the", "updated the", "posted the",
	"i've set", "i set", "i've scheduled", "i scheduled", "i've assigned", "i assigned",
}

// failureAcknowledgedPhrases indicate the draft ALREADY owns a failure/partial
// outcome, so it is being honest and needs no correction.
var failureAcknowledgedPhrases = []string{
	"couldn't", "could not", "wasn't able", "was not able", "unable to",
	"failed", "didn't", "did not", "not able to", "ran into", "went wrong",
	"try again", "retry", "needs a retry", "may need", "manually", "permission",
	"couldn’t", // curly apostrophe variant
}

// claimVerificationVerdict is the result of the deterministic honesty gate.
type claimVerificationVerdict struct {
	NeedsCorrection bool
	// FailedTools carries the humanized failed-write tool names for the
	// correction turn (so the model is told exactly what to redo or disclose).
	FailedTools []string
}

// verifyRunClaims reports whether a draft final answer contradicts the run's
// ground truth: it asserts the work is DONE while at least one WRITE tool it
// ran ERRORED, and it does not acknowledge any failure. failedWriteTools is the
// already-filtered list of write (non-read-only) tools whose calls errored.
// Pure + DB-free so it is unit-testable and model-agnostic. Returns a zero
// verdict (no correction) whenever nothing failed, the draft is empty, the
// draft doesn't claim success, or the draft already owns a failure.
func verifyRunClaims(draft string, failedWriteTools []string) claimVerificationVerdict {
	if len(failedWriteTools) == 0 {
		return claimVerificationVerdict{}
	}
	s := strings.ToLower(strings.TrimSpace(draft))
	if s == "" {
		return claimVerificationVerdict{}
	}
	// Already honest about a failure → leave it alone (don't nag a correct answer).
	for _, p := range failureAcknowledgedPhrases {
		if strings.Contains(s, p) {
			return claimVerificationVerdict{}
		}
	}
	// Only correct a draft that actually asserts completion.
	claimsSuccess := false
	for _, p := range successAssertionPhrases {
		if strings.Contains(s, p) {
			claimsSuccess = true
			break
		}
	}
	if !claimsSuccess {
		return claimVerificationVerdict{}
	}
	return claimVerificationVerdict{NeedsCorrection: true, FailedTools: failedWriteTools}
}

// claimCorrectionPrefix opens every claim-correction turn. Named so the
// compaction glue can recognise this turn as loop scaffolding (not something a
// human said) without duplicating the wording — change the text once, here.
const claimCorrectionPrefix = "Your reply claims the work is done, but these actions FAILED"

// claimCorrection is the corrective user turn injected when a draft claims
// success over a failed write. It is concrete about which actions failed so even
// a weak model either retries the action or rewrites the reply honestly.
func claimCorrection(failedWriteTools []string) string {
	names := make([]string, 0, len(failedWriteTools))
	seen := map[string]bool{}
	for _, t := range failedWriteTools {
		h := humanizeToolName(t)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		names = append(names, h)
	}
	list := strings.Join(names, ", ")
	return claimCorrectionPrefix + " and did NOT take effect: " + list + ". " +
		"Do NOT claim success for an action that failed. Either retry it NOW by emitting the tool call again, " +
		"or rewrite your reply to state honestly what actually completed and what did not (and what a person should do next). " +
		"Never report a failed write as done."
}

// succeededWriteTools filters a run's succeeded-tool list down to WRITEs. The
// mirror of failedWriteTools, and the ground truth for "did this run actually
// change anything".
func succeededWriteTools(toolsSucceeded []string) []string {
	out := make([]string, 0, len(toolsSucceeded))
	for _, t := range toolsSucceeded {
		if !ai.ToolIsReadOnly(t) {
			out = append(out, t)
		}
	}
	return out
}

// writeAssertionPhrases are claims that something was CHANGED, as opposed to
// found or explained.
//
// Deliberately narrower than successAssertionPhrases. "done", "all set" and
// "successfully" are perfectly honest after a read ("Done, here are the three
// open tasks"), so flagging them would punish correct answers. Only unambiguous
// mutation language belongs here.
var writeAssertionPhrases = []string{
	"i've created", "i have created", "i created", "has been created", "have been created",
	"i've updated", "i have updated", "i updated", "has been updated", "have been updated",
	"i've posted", "i have posted", "i posted", "has been posted",
	"i've sent", "i have sent", "i sent", "has been sent",
	"i've added", "i have added", "i added", "has been added",
	"i've assigned", "i have assigned", "i assigned", "has been assigned",
	"i've scheduled", "i have scheduled", "i scheduled", "has been scheduled",
	"i've deleted", "i have deleted", "i deleted", "has been deleted",
	"i've moved", "i have moved", "i moved", "has been moved",
	"task created", "created the", "updated the", "posted the", "assigned the",
}

// verifyWorkHappened catches the failure the gate above cannot see: the draft
// says it CHANGED something and no write tool ran at all.
//
// verifyRunClaims only fires when a write was ATTEMPTED and errored. That is the
// rarer case. The common and more damaging one is an agent that reads a few
// things, never calls a write tool, and reports "I've updated the doc" anyway.
// Nothing failed, so there is no failure to contradict; something succeeded, so
// the stall guard does not fire; and confident prose does not look like a
// non-answer. The run was recorded as succeeded and the work did not exist.
//
// The check is the same shape as its sibling and just as cheap: it compares the
// draft's own mutation language against the run's execution ledger, with no
// model call and nothing the prose can talk its way past.
//
// Read-only runs are the normal case and must stay silent, which is what the
// narrower phrase list buys: an answer that merely REPORTS a change somebody
// else made ("the task was assigned to Sam last week") uses past-tense
// description, not first-person completion.
func verifyWorkHappened(draft string, succeededWrites []string) claimVerificationVerdict {
	// Something was genuinely written, so the honesty question is the sibling's,
	// not this one's.
	if len(succeededWrites) > 0 {
		return claimVerificationVerdict{}
	}
	s := strings.ToLower(strings.TrimSpace(draft))
	if s == "" {
		return claimVerificationVerdict{}
	}
	// Already owns a failure or a limitation → honest, leave it alone.
	for _, p := range failureAcknowledgedPhrases {
		if strings.Contains(s, p) {
			return claimVerificationVerdict{}
		}
	}
	for _, p := range writeAssertionPhrases {
		if strings.Contains(s, p) {
			return claimVerificationVerdict{NeedsCorrection: true}
		}
	}
	return claimVerificationVerdict{}
}

// noWriteCorrection is the single nudge for a draft that claimed a change no
// tool made. Names the ground truth rather than accusing, so the model can
// either perform the action it skipped or restate what it actually did.
const noWriteCorrection = "Your draft says you created, updated, sent or assigned something, but this run has not " +
	"successfully called any tool that changes data. Either call the tool that performs the action now, or rewrite " +
	"the reply to describe only what you actually did, and say plainly that the change was not made."
