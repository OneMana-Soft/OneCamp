package ai

// Conversation compaction for long tool-loops.
//
// WHY THIS EXISTS
// ---------------
// A tool-loop conversation only grows: every step appends the assistant turn
// plus one observation per tool call. Nothing removed anything, so a long run
// walked straight into the model's context window and the failure was ugly:
//   - Ollama silently truncates from the FRONT (dropping the system prompt and
//     the tool instructions first), so the agent quietly forgets its own rules;
//   - cloud providers return a hard 400/413 ("context length exceeded", "prompt
//     is too long") and the whole run failed with "the AI model call failed",
//     losing every completed step.
// Clipping each observation (truncateObservation) bounds ONE result but not the
// TOTAL, so it only delays the wall.
//
// WHAT THIS DOES
// --------------
// When the outbound conversation approaches the usable input budget, the OLDER
// portion is replaced by a single block containing:
//  1. an LLM-written structured summary of the folded turns (what was asked,
//     what was decided, what is in flight, what is next);
//  2. mechanically extracted working state — writes that landed, reads used,
//     targets touched, failures — taken from the tool RECORD, not the model's
//     narration, so it cannot hallucinate;
//  3. every real human message in the folded span, verbatim (trimmed), because
//     user intent is ground truth and must not depend on a model remembering
//     to include it.
// The most recent turns are always kept verbatim, and the system prompt is
// never touched.
//
// DESIGN NOTES
// ------------
//   - Pure functions plus exactly one provider call. The caller owns WHEN to
//     compact and WITH WHAT model, so every policy here is testable without a
//     provider.
//   - Provider-legal by construction: the folded span always ends at a message
//     that may legally head a conversation (a user turn or an assistant turn).
//     A `tool` result is never a boundary, so an assistant turn carrying
//     tool_calls can never be separated from the results that answer it — the
//     orphaned-tool_call 400 that would otherwise break the native path.
//   - Never makes things worse: if the compacted view is not smaller than the
//     original it degrades to a mechanical trim, and if that is still not
//     smaller it declines to compact at all (the caller keeps the original).
//   - Degrades safely: a failed/empty summarizer call falls back to the
//     mechanical block, which is free and deterministic.
//   - Generic: it knows nothing about agents. Any ChatMessage tool-loop (the
//     agent runner today, the coworker/assistant loops tomorrow) can use it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Message roles used by the compaction logic. Roles are plain strings on the
// wire; these constants keep the comparisons honest in one place.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

const (
	// compactedBlockOpen / compactedBlockClose delimit the stand-in block. The
	// open tag is also how a repeat compaction recognises its own prior block
	// (so it folds the turns SINCE it, rather than re-folding the block).
	compactedBlockOpen  = "<compacted-history>"
	compactedBlockClose = "</compacted-history>"

	// defaultCompactionThresholdPct is the share of the usable input budget at
	// which compaction triggers. 0.75 leaves room for the next step's assistant
	// turn plus its observations, so we compact BEFORE the wall rather than
	// after a provider rejection.
	defaultCompactionThresholdPct = 0.75

	// defaultCompactionCapTokens caps the trigger for very large windows: a
	// 200k+ context model degrades in quality and latency (and cost) long
	// before its nominal limit, so compact well short of it.
	defaultCompactionCapTokens = 250000

	// compactionKeepFraction is the share of the trigger kept VERBATIM as the
	// most recent tail. A token budget rather than a turn count, so one huge
	// tool loop can't starve the working set. The remainder of the trigger is
	// headroom for the block plus the next few steps, which prevents
	// compacting again on the very next step (thrash).
	compactionKeepFraction = 0.35

	// Summary sizing. The summarizer runs on the SAME model, so its output must
	// be small enough to live inside the window it is protecting.
	compactionMinSummaryTokens = 256
	compactionMaxSummaryTokens = 1500

	// compactionUserClipChars bounds one preserved human message (pasted bulk
	// is what makes these huge; the intent survives the clip).
	compactionUserClipChars = 400
	// compactionUserMessagesMax caps how many preserved human messages the
	// block carries across repeated compactions — otherwise the list grows
	// forever and slowly reclaims the window compaction just freed. Dropped
	// ones stay COUNTED so the block stays honest about the omission.
	compactionUserMessagesMax = 20

	// compactionSpanResultClip bounds one tool observation when rendering the
	// span for the summarizer. Tool results are the first casualty: they are
	// large and mostly stale (a file listing from 30 steps ago is better
	// re-read than remembered).
	compactionSpanResultClip = 400

	// compactionTargetClipChars bounds one extracted target label (a repo, a
	// path, a title) in the mechanical working-state block.
	compactionTargetClipChars = 120
	// compactionMaxTargets / compactionMaxFailures bound the mechanical lists.
	compactionMaxTargets  = 15
	compactionMaxFailures = 8
)

// errEmptySummary is returned when the summarizer produced no usable text; the
// caller degrades to the mechanical block.
var errEmptySummary = errors.New("ai: compaction summarizer returned an empty summary")

// CompactionState is one compaction point: the notes that stand in for every
// turn folded into the block. It is carried forward by the caller so a repeat
// compaction folds its prior summary into the new one instead of losing it, and
// so the caller can report/persist what happened.
type CompactionState struct {
	// Summary is the LLM-written structured summary of the folded turns (or a
	// plain notice when Trimmed).
	Summary string `json:"summary"`
	// WorkingState is the mechanically extracted, hallucination-proof block
	// (writes, reads, targets, failures) — derived from the tool record.
	WorkingState string `json:"working_state,omitempty"`
	// UserMessages are the real human messages of every folded span, verbatim
	// (whitespace-normalised and clipped), oldest first.
	UserMessages []string `json:"user_messages,omitempty"`
	// UserMessagesDropped counts human messages the cap dropped across all
	// compactions of this conversation, so the block can say how many are
	// omitted rather than silently losing them.
	UserMessagesDropped int `json:"user_messages_dropped,omitempty"`
	// Folded is the cumulative number of messages replaced by the block.
	Folded int `json:"folded"`
	// Rounds is how many times this conversation has been compacted.
	Rounds int `json:"rounds"`
	// TokensBefore / TokensAfter are the estimated outbound size either side of
	// the most recent compaction (observability: proves it actually shrank).
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	// Trimmed is true when the summarizer was unavailable and the block carries
	// only mechanical state plus a notice.
	Trimmed bool `json:"trimmed,omitempty"`
	// Model is the model that wrote the summary (empty when trimmed).
	Model     string    `json:"model,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CompactOptions configures one compaction. Every field is optional: the zero
// value compacts a conversation using the workspace's configured window.
type CompactOptions struct {
	// Prior is the state from this conversation's previous compaction (nil the
	// first time). Its summary is folded into the new one.
	Prior *CompactionState
	// ResponseTokens is the caller's per-call output reservation (its
	// ChatOptions.MaxTokens), used to derive the usable input budget.
	ResponseTokens int
	// KeepTokens overrides the verbatim-tail budget. 0 derives it.
	KeepTokens int
	// MaxSummaryTokens overrides the summarizer's output ceiling. 0 derives it.
	MaxSummaryTokens int
	// SyntheticUser reports whether a `user` message is loop scaffolding (a
	// "tool results" turn, a corrective nudge) rather than something a human
	// said. Scaffolding is summarized like any other turn but is NEVER
	// preserved in the verbatim human-intent list. nil treats every user
	// message as human.
	SyntheticUser func(ChatMessage) bool
	// Model labels the summarizer model for the run record. Optional.
	Model string
}

// CompactResult is the outcome of one compaction attempt.
type CompactResult struct {
	// Messages is the conversation to send from now on: unchanged when
	// Compacted is false, otherwise [system?] + block + verbatim tail.
	Messages []ChatMessage
	// State is the new compaction state (nil when nothing was compacted). Pass
	// it back as CompactOptions.Prior on the next compaction.
	State *CompactionState
	// Compacted reports whether Messages was actually replaced.
	Compacted bool
	// SummaryErr is the summarizer failure, when the block fell back to
	// mechanical state only. Advisory: the compaction still succeeded.
	SummaryErr error
}

// -- budget math --------------------------------------------------------------

// EstimateMessageTokens is a conservative token estimate for one message,
// including native tool-call payloads (arguments are often larger than the
// visible text) and per-message framing overhead.
func EstimateMessageTokens(m ChatMessage) int {
	return WorkspaceLimits().EstimateMessageTokens(m)
}

// EstimateMessageTokens is the conservative estimate for one message under THIS
// model's tokenizer, including native tool-call payloads and framing overhead.
func (l ModelLimits) EstimateMessageTokens(m ChatMessage) int {
	t := l.EstimateTokens(m.Content) + 4
	if m.Name != "" {
		t += l.EstimateTokens(m.Name)
	}
	for _, tc := range m.ToolCalls {
		t += l.EstimateTokens(tc.Name) + l.EstimateTokens(tc.Arguments) + 8
	}
	return t
}

// EstimateMessagesTokens is the conservative estimate for a whole conversation
// under THIS model's tokenizer — the signal every compaction decision is made on.
func (l ModelLimits) EstimateMessagesTokens(msgs []ChatMessage) int {
	total := 0
	for _, m := range msgs {
		total += l.EstimateMessageTokens(m)
	}
	return total
}

// EstimateMessagesTokens is the conservative token estimate for a whole
// conversation — the signal every compaction decision is made on.
func EstimateMessagesTokens(msgs []ChatMessage) int {
	total := 0
	for _, m := range msgs {
		total += EstimateMessageTokens(m)
	}
	return total
}

// ConversationInputBudget is the tokens available for the OUTBOUND messages of a
// chat call on THIS model that reserves responseTokens for its answer.
func (l ModelLimits) ConversationInputBudget(responseTokens int) int {
	avail := l.ContextWindow - l.ResponseReserve(responseTokens) - 256 // 256: request framing
	if avail < 512 {
		avail = 512
	}
	return avail
}

// CompactionTriggerTokens is the outbound size at which a conversation on THIS model
// should be compacted. The fraction and the absolute cap stay model-independent on
// purpose: they express when a conversation is too long to steer well, which is a
// property of long conversations rather than of any one model. What was wrong before
// was the WINDOW they were fractions of.
func (l ModelLimits) CompactionTriggerTokens(responseTokens int) int {
	pct := defaultCompactionThresholdPct
	if v := envFloat("AI_CONTEXT_COMPACT_THRESHOLD_PCT"); v > 0 && v <= 1 {
		pct = v
	}
	capTokens := defaultCompactionCapTokens
	if v := getEnvInt("AI_CONTEXT_COMPACT_CAP_TOKENS", 0); v > 0 {
		capTokens = v
	}
	trigger := int(float64(l.ConversationInputBudget(responseTokens)) * pct)
	if trigger > capTokens {
		trigger = capTokens
	}
	if trigger < 512 {
		trigger = 512
	}
	return trigger
}

// CompactionKeepTokens is the verbatim-tail budget for THIS model.
func (l ModelLimits) CompactionKeepTokens(responseTokens int) int {
	keep := int(float64(l.CompactionTriggerTokens(responseTokens)) * compactionKeepFraction)
	if keep < 256 {
		keep = 256
	}
	return keep
}

// compactionSummaryTokens is the summarizer's output ceiling, scaled to the
// window it is protecting (a small local window can't afford a 1500-token
// summary). Tunable with AI_CONTEXT_COMPACT_SUMMARY_TOKENS.
func (l ModelLimits) compactionSummaryTokens(responseTokens int) int {
	if v := getEnvInt("AI_CONTEXT_COMPACT_SUMMARY_TOKENS", 0); v > 0 {
		return v
	}
	n := l.CompactionTriggerTokens(responseTokens) / 8
	if n < compactionMinSummaryTokens {
		n = compactionMinSummaryTokens
	}
	if n > compactionMaxSummaryTokens {
		n = compactionMaxSummaryTokens
	}
	return n
}

// minCompactionSpanTokens is the smallest span worth summarizing. Below it the
// summarizer call costs more (latency, tokens) than the space it frees, and the
// next step would just trigger another one.
func (l ModelLimits) minCompactionSpanTokens(responseTokens int) int {
	n := l.CompactionTriggerTokens(responseTokens) / 8
	if n < 256 {
		n = 256
	}
	return n
}

// ShouldCompactConversation reports whether the conversation has grown past the
// trigger for a call that reserves responseTokens for its answer, measured against
// the limits of the model serving ctx (the workspace default when ctx carries none).
//
// Takes a context precisely so this decision cannot be made against the wrong model:
// deciding too late overflows the window and the provider rejects the call, deciding
// too early throws away context the model could have held. Both are silent.
func ShouldCompactConversation(ctx context.Context, msgs []ChatMessage, responseTokens int) bool {
	if len(msgs) < 3 {
		return false
	}
	lim := LimitsFrom(ctx)
	return lim.EstimateMessagesTokens(msgs) >= lim.CompactionTriggerTokens(responseTokens)
}

// -- boundary selection -------------------------------------------------------

// PickCompactionBoundary returns the index where the verbatim tail must begin:
// the EARLIEST turn start at or after `start` whose suffix fits keepTokens (so
// as much recent context as possible survives). Returns -1 when there is
// nothing worth folding.
//
// Only a `user` or `assistant` message may head the tail. A `tool` message is
// never a candidate: heading the outbound view with a tool result would orphan
// it from the assistant turn that requested it, which providers reject.
func PickCompactionBoundary(msgs []ChatMessage, start, keepTokens int) int {
	if start < 0 {
		start = 0
	}
	if start >= len(msgs) || keepTokens <= 0 {
		return -1
	}
	var users, assistants []int
	for i := start; i < len(msgs); i++ {
		switch msgs[i].Role {
		case roleUser:
			users = append(users, i)
		case roleAssistant:
			assistants = append(assistants, i)
		}
	}
	fit := func(candidates []int) int {
		for _, i := range candidates { // earliest first: keep the most verbatim
			if EstimateMessagesTokens(msgs[i:]) <= keepTokens {
				return i
			}
		}
		return -1
	}

	boundary := fit(users)
	if boundary < 0 && len(users) > 0 {
		// The newest turn alone exceeds the keep budget (one huge tool loop):
		// cut INSIDE it at a step boundary, keeping the most recent step.
		var inside []int
		for _, i := range assistants {
			if i > users[len(users)-1] {
				inside = append(inside, i)
			}
		}
		if boundary = fit(inside); boundary < 0 {
			if len(inside) > 0 {
				boundary = inside[len(inside)-1]
			} else {
				boundary = users[len(users)-1]
			}
		}
	}
	if boundary < 0 {
		if boundary = fit(assistants); boundary < 0 && len(assistants) > 0 {
			boundary = assistants[len(assistants)-1]
		}
	}
	if boundary <= start {
		return -1 // folding nothing (or a single message) buys nothing
	}
	return boundary
}

// -- mechanical extraction (no LLM: zero hallucination risk) ------------------

// spanToolCall is one tool invocation recovered from a folded span, with its
// observation when one can be matched.
type spanToolCall struct {
	Name   string
	Params map[string]string
	Result string
}

// targetParamKeys is the priority order for naming WHAT a tool call acted on.
// Deliberately generic: it works for any connector or MCP server rather than
// enumerating tools, so a new tool needs no change here.
var targetParamKeys = []string{
	"path", "file_path", "repo", "repository", "full_name", "owner",
	"url", "title", "name", "instruction", "query",
	"task_uuid", "project_uuid", "channel_uuid", "doc_uuid", "id",
}

// spanToolCalls recovers every tool call in a span, in order, from BOTH loop
// protocols: native structured tool_calls (results matched by tool_call_id) and
// the text `<tool_call>` protocol (results are only available as text, so they
// are matched by the "<tool> -> <observation>" lines the loop feeds back).
func spanToolCalls(span []ChatMessage) []spanToolCall {
	byID := make(map[string]string)
	textResults := make(map[string]string)
	for _, m := range span {
		if m.Role == roleTool && m.ToolCallID != "" {
			byID[m.ToolCallID] = m.Content
			continue
		}
		if m.Role == roleUser {
			for _, line := range strings.Split(m.Content, "\n") {
				name, obs, ok := strings.Cut(strings.TrimSpace(line), " -> ")
				if ok && name != "" && !strings.Contains(name, " ") {
					textResults[name] = obs
				}
			}
		}
	}

	var out []spanToolCall
	for _, m := range span {
		if m.Role != roleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			a := ToolCallToAction(tc)
			out = append(out, spanToolCall{Name: a.ToolName, Params: a.Params, Result: byID[tc.ID]})
		}
		if len(m.ToolCalls) == 0 && strings.Contains(m.Content, "<tool_call>") {
			if _, acts := ParseToolCalls(m.Content); len(acts) > 0 {
				for _, a := range acts {
					out = append(out, spanToolCall{Name: a.ToolName, Params: a.Params, Result: textResults[a.ToolName]})
				}
			}
		}
	}
	return out
}

// callTarget names what a call acted on, from the first populated priority
// param. Empty when the call has no recognisable target (a plain read).
func callTarget(params map[string]string) string {
	for _, k := range targetParamKeys {
		if v := strings.TrimSpace(params[k]); v != "" {
			return clipText(strings.Join(strings.Fields(v), " "), compactionTargetClipChars)
		}
	}
	return ""
}

// ExtractWorkingState renders the hallucination-proof half of the block: what
// the folded turns actually DID, taken from the tool record rather than the
// model's prose. Returns "" when the span contains no tool activity.
//
// Writes and reads are separated using the tool registry (ToolIsReadOnly), not
// name heuristics, so it stays correct for every connector and MCP tool.
func ExtractWorkingState(span []ChatMessage) string {
	calls := spanToolCalls(span)
	if len(calls) == 0 {
		return ""
	}
	var writes, reads, targets, failures []string
	seenWrite, seenRead, seenTarget, seenFail := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range calls {
		if c.Name == "" {
			continue
		}
		failed := strings.HasPrefix(strings.TrimSpace(strings.ToLower(c.Result)), "error:")
		switch {
		case failed:
			line := c.Name
			if t := callTarget(c.Params); t != "" {
				line += " (" + t + ")"
			}
			if !seenFail[line] {
				seenFail[line] = true
				failures = append(failures, line)
			}
		case ToolIsReadOnly(c.Name):
			if !seenRead[c.Name] {
				seenRead[c.Name] = true
				reads = append(reads, c.Name)
			}
		default:
			if !seenWrite[c.Name] {
				seenWrite[c.Name] = true
				writes = append(writes, c.Name)
			}
		}
		if t := callTarget(c.Params); t != "" && !seenTarget[t] {
			seenTarget[t] = true
			targets = append(targets, t)
		}
	}

	lines := []string{"## Working state (from the tool record, not from narration)"}
	if len(writes) > 0 {
		lines = append(lines, "Actions that ran: "+strings.Join(writes, ", "))
	}
	if len(reads) > 0 {
		lines = append(lines, "Lookups used: "+strings.Join(reads, ", "))
	}
	if len(targets) > 0 {
		lines = append(lines, "Things touched (in order):")
		for _, t := range newestFirst(targets, compactionMaxTargets) {
			lines = append(lines, "- "+t)
		}
	}
	if len(failures) > 0 {
		lines = append(lines, "Calls that FAILED (do not claim these as done):")
		for _, f := range newestFirst(failures, compactionMaxFailures) {
			lines = append(lines, "- "+f)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// ExtractUserIntent returns the real human messages of a span, verbatim
// (whitespace-normalised and clipped), oldest first. Loop scaffolding is
// excluded via the caller's predicate: it is summarized like any other turn,
// but it is not something a person said.
func ExtractUserIntent(span []ChatMessage, synthetic func(ChatMessage) bool) []string {
	var out []string
	for _, m := range span {
		if m.Role != roleUser {
			continue
		}
		if synthetic != nil && synthetic(m) {
			continue
		}
		text := strings.Join(strings.Fields(m.Content), " ")
		if text == "" || strings.HasPrefix(text, compactedBlockOpen) {
			continue
		}
		out = append(out, clipText(text, compactionUserClipChars))
	}
	return out
}

// capUserMessages keeps the NEWEST messages that fit both the count cap and the
// token budget, returning the kept slice and the running total of everything
// ever dropped (so the block can disclose the omission).
func capUserMessages(all []string, priorDropped, maxTokens int) ([]string, int) {
	kept := all
	dropped := priorDropped
	if len(kept) > compactionUserMessagesMax {
		dropped += len(kept) - compactionUserMessagesMax
		kept = kept[len(kept)-compactionUserMessagesMax:]
	}
	if maxTokens <= 0 {
		return kept, dropped
	}
	total, start := 0, len(kept)
	for i := len(kept) - 1; i >= 0; i-- {
		t := EstimateTokens(kept[i]) + 2
		if total+t > maxTokens {
			break
		}
		total += t
		start = i
	}
	if start > 0 {
		dropped += start
		kept = kept[start:]
	}
	return kept, dropped
}

// -- summarizer ---------------------------------------------------------------

// compactionSystemPrompt asks for a compact, load-bearing summary. It is
// deliberately shorter than a "write me a report" prompt: the same model that
// runs the loop writes this, often a small local one, and the output has to fit
// inside the window it is protecting.
const compactionSystemPrompt = `You are compacting an assistant's working history so it can keep working in a smaller context. Write a summary of the conversation below. It becomes the assistant's ONLY memory of these turns, so keep everything load-bearing and drop everything else.

Use exactly these markdown sections, in order:

## Request and intent
What the human wants, in their terms, including any standing constraint they stated (e.g. "always ask me first"). Constraints outlive the turn they were stated in.

## Decisions and facts
Identifiers, names, choices made and WHY. Be concrete: ids, paths, repos, keys — never vague references.

## Done
What has actually been completed, per the tool results. Never record a failed action as done.

## Open
What is unfinished, promised, or blocked.

## Current work and next step
Precisely where the work stands and the single next action.

Rules:
- Do not copy file contents or long tool output as truth; note THAT something was read or changed. Stale remembered content is worse than none — it can be re-read.
- Do not invent progress. If the turns show a failure, say so.
- Output only the sections, no preamble.`

// compactionContinuationContract tells the model how to behave after a
// compaction: resume, don't recap, don't apologise, don't mention it.
const compactionContinuationContract = "Continue from the current work and next step above. Do not re-ask something already answered, " +
	"do not recap what happened, and do not mention this summary or that context was compacted. If you need content that is " +
	"not in these notes (a file, a search result), fetch it again with a tool."

// renderSpan renders a folded span as compact text for the summarizer. Tool
// results are clipped hard (large and mostly stale); if the whole render still
// exceeds the budget the OLDEST lines are dropped, since the newest turns are
// the most load-bearing.
func renderSpan(span []ChatMessage, maxTokens int) string {
	var lines []string
	for _, m := range span {
		switch m.Role {
		case roleSystem:
			continue
		case roleTool:
			text := strings.Join(strings.Fields(m.Content), " ")
			lines = append(lines, "[tool result "+m.Name+"] "+clipText(text, compactionSpanResultClip))
		case roleAssistant:
			for _, tc := range m.ToolCalls {
				args := strings.Join(strings.Fields(tc.Arguments), " ")
				lines = append(lines, "[assistant calls "+tc.Name+"] "+clipText(args, 200))
			}
			if text := strings.TrimSpace(m.Content); text != "" {
				lines = append(lines, "[assistant] "+text)
			}
		case roleUser:
			if text := strings.TrimSpace(m.Content); text != "" {
				lines = append(lines, "[user] "+text)
			}
		}
	}
	rendered := strings.Join(lines, "\n")
	if maxTokens > 0 && EstimateTokens(rendered) > maxTokens {
		rendered = "(…oldest turns elided…)\n" + tailToTokenBudget(rendered, maxTokens)
	}
	return rendered
}

// summarizeSpan performs the single provider round-trip. Tools are not
// advertised (this is a plain text call on every provider), so the summarizer
// behaves identically whichever protocol the loop itself uses.
func summarizeSpan(ctx context.Context, llm LLMProvider, span []ChatMessage, priorSummary string, maxTokens, spanTokens int) (string, error) {
	if llm == nil {
		return "", errors.New("ai: no provider for compaction")
	}
	body := renderSpan(span, spanTokens)
	if strings.TrimSpace(priorSummary) != "" {
		body = "[summary of even earlier turns — fold anything still relevant into the new summary]\n" +
			priorSummary + "\n\n[conversation since]\n" + body
	}
	if strings.TrimSpace(body) == "" {
		return "", errEmptySummary
	}
	ans, err := llm.Chat(ctx, []ChatMessage{
		{Role: roleSystem, Content: compactionSystemPrompt},
		{Role: roleUser, Content: body},
	}, ChatOptions{Temperature: 0.2, MaxTokens: maxTokens})
	if err != nil {
		return "", err
	}
	clean, _ := ParseToolCalls(StripReasoning(ans))
	if clean = strings.TrimSpace(clean); clean == "" {
		return "", errEmptySummary
	}
	return clean, nil
}

// -- block + application ------------------------------------------------------

// CompactedBlock renders the single message that stands in for every folded
// turn: the summary, the mechanical working state, the human's own words, and
// the continuation contract.
func CompactedBlock(state *CompactionState) string {
	if state == nil {
		return ""
	}
	parts := []string{
		compactedBlockOpen,
		"Earlier turns of this session were compacted to fit the context window. These notes are your memory of them.",
		"",
		strings.TrimSpace(state.Summary),
	}
	if ws := strings.TrimSpace(state.WorkingState); ws != "" {
		parts = append(parts, "", ws)
	}
	if len(state.UserMessages) > 0 {
		parts = append(parts, "", "## What the human said (verbatim, oldest first)")
		if state.UserMessagesDropped > 0 {
			parts = append(parts, fmt.Sprintf("(%d earlier message(s) omitted — their intent is covered above)", state.UserMessagesDropped))
		}
		for _, u := range state.UserMessages {
			parts = append(parts, "- "+u)
		}
	}
	parts = append(parts, "", compactionContinuationContract, compactedBlockClose)
	return strings.Join(parts, "\n")
}

// IsCompactedBlock reports whether a message is a compaction stand-in block.
func IsCompactedBlock(m ChatMessage) bool {
	return m.Role == roleUser && strings.HasPrefix(strings.TrimSpace(m.Content), compactedBlockOpen)
}

// CompactConversation folds the older part of a conversation into a summary
// block and returns the conversation to send from now on.
//
// It never returns an error: a failed summarizer degrades to a mechanical
// (summary-free) block, and a compaction that wouldn't actually shrink the
// conversation is declined (Compacted=false, Messages unchanged), so a caller
// can always use the result as-is.
func CompactConversation(ctx context.Context, llm LLMProvider, msgs []ChatMessage, opts CompactOptions) CompactResult {
	res := CompactResult{Messages: msgs}
	if len(msgs) < 3 {
		return res
	}

	head := 0
	if msgs[0].Role == roleSystem {
		head = 1
	}
	// A previous compaction's block sits immediately after the system prompt.
	// Fold the turns SINCE it and carry its notes forward, rather than
	// re-summarizing a summary of a summary.
	spanStart := head
	priorSummary := ""
	var priorUsers []string
	priorDropped, priorFolded, priorRounds := 0, 0, 0
	if head < len(msgs) && IsCompactedBlock(msgs[head]) {
		spanStart = head + 1
		if opts.Prior != nil {
			priorSummary = opts.Prior.Summary
			priorUsers = opts.Prior.UserMessages
			priorDropped = opts.Prior.UserMessagesDropped
			priorFolded = opts.Prior.Folded
			priorRounds = opts.Prior.Rounds
		}
	}

	// The limits of the model whose window this compaction is protecting. Read from the
	// context rather than the global config: the summarizer runs on the SAME model as
	// the conversation, so every budget below — what to keep verbatim, how long the
	// summary may be, how much span the summarizer can read — has to be sized against
	// that model and not against whichever one the workspace happens to default to.
	lim := LimitsFrom(ctx)

	keep := opts.KeepTokens
	if keep <= 0 {
		keep = lim.CompactionKeepTokens(opts.ResponseTokens)
	}
	boundary := PickCompactionBoundary(msgs, spanStart, keep)
	if boundary < 0 {
		return res
	}
	span := msgs[spanStart:boundary]
	// Folding a couple of small turns costs a summarizer call and saves almost
	// nothing — and would repeat on the very next step. When most of the prompt
	// is the system message (a big instruction set on a small window), there is
	// simply nothing useful to compact; say so and let the caller proceed.
	if len(span) < 2 || lim.EstimateMessagesTokens(span) < lim.minCompactionSpanTokens(opts.ResponseTokens) {
		return res
	}

	maxSummary := opts.MaxSummaryTokens
	if maxSummary <= 0 {
		maxSummary = lim.compactionSummaryTokens(opts.ResponseTokens)
	}
	// The summarizer runs on the same model: its INPUT must fit the window too.
	spanBudget := lim.ConversationInputBudget(maxSummary) - lim.EstimateTokens(compactionSystemPrompt) - 128
	if spanBudget < 256 {
		spanBudget = 256
	}

	before := lim.EstimateMessagesTokens(msgs)
	state := &CompactionState{
		WorkingState: ExtractWorkingState(span),
		Folded:       priorFolded + len(span),
		Rounds:       priorRounds + 1,
		TokensBefore: before,
		Model:        opts.Model,
		UpdatedAt:    time.Now().UTC(),
	}
	state.UserMessages, state.UserMessagesDropped = capUserMessages(
		append(append([]string{}, priorUsers...), ExtractUserIntent(span, opts.SyntheticUser)...),
		priorDropped, maxSummary/2,
	)

	summary, serr := summarizeSpan(ctx, llm, span, priorSummary, maxSummary, spanBudget)
	if serr != nil {
		res.SummaryErr = serr
		state.Trimmed = true
		state.Model = ""
		state.Summary = compactionTrimNotice(priorSummary)
	} else {
		state.Summary = summary
	}

	build := func() []ChatMessage {
		out := make([]ChatMessage, 0, len(msgs)-len(span)+2)
		out = append(out, msgs[:head]...)
		out = append(out, ChatMessage{Role: roleUser, Content: CompactedBlock(state)})
		return append(out, msgs[boundary:]...)
	}
	out := build()
	after := EstimateMessagesTokens(out)
	if after >= before && !state.Trimmed {
		// A summary that costs more than the turns it replaced is worse than no
		// summary: keep the free mechanical state and drop the prose.
		state.Trimmed = true
		state.Model = ""
		state.Summary = compactionTrimNotice(priorSummary)
		out = build()
		after = EstimateMessagesTokens(out)
	}
	if after >= before {
		// Nothing to gain — leave the conversation exactly as it was so the
		// caller can decide (e.g. stop cleanly) instead of looping.
		return res
	}
	state.TokensAfter = after
	res.Messages, res.State, res.Compacted = out, state, true
	return res
}

// compactionTrimNotice is the summary-free stand-in text: it must be honest
// that detail was lost and tell the model how to recover it.
func compactionTrimNotice(priorSummary string) string {
	notice := "Older turns were dropped to fit the context window and no summary of them is available. " +
		"Rely on the working state below; re-read files or re-run lookups if you need earlier results."
	if s := strings.TrimSpace(priorSummary); s != "" {
		return s + "\n\n" + notice
	}
	return notice
}

// -- overflow rescue ----------------------------------------------------------

// contextOverflowMarkers are the phrases providers use when the prompt exceeds
// the model's context window. Matched case-insensitively on the error text
// because there is no portable status code for it (OpenAI/Groq return 400,
// others 413, Ollama fails differently again).
var contextOverflowMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"context length exceeded",
	"context window",
	"prompt is too long",
	"input is too long",
	"too many tokens",
	"reduce the length of the messages",
	"exceeds the maximum number of tokens",
	"requested too many tokens",
}

// IsContextOverflow reports whether err is a provider rejection caused by the
// prompt exceeding the model's context window — the case a caller should answer
// by compacting and retrying, not by failing the run.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, m := range contextOverflowMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// -- small helpers ------------------------------------------------------------

// clipText caps s to max runes, marking the cut. Rune-safe.
func clipText(s string, max int) string {
	if max <= 1 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// tailToTokenBudget keeps the NEWEST portion of s that fits maxTokens, cutting
// on a line boundary when one is close by. Used for the summarizer's view of a
// span, where recent lines matter most (the mirror of TruncateToTokenBudget,
// which keeps the prefix).
func tailToTokenBudget(s string, maxTokens int) string {
	if maxTokens <= 0 || EstimateTokens(s) <= maxTokens {
		return s
	}
	r := []rune(s)
	keep := maxTokens * avgCharsPerToken
	if keep >= len(r) {
		return s
	}
	tail := string(r[len(r)-keep:])
	if nl := strings.Index(tail, "\n"); nl >= 0 && nl < len(tail)/4 {
		tail = tail[nl+1:]
	}
	return tail
}

// newestFirst returns up to limit de-duplicated items, most recent first.
func newestFirst(items []string, limit int) []string {
	out := make([]string, 0, limit)
	seen := make(map[string]bool, len(items))
	for i := len(items) - 1; i >= 0 && len(out) < limit; i-- {
		if seen[items[i]] {
			continue
		}
		seen[items[i]] = true
		out = append(out, items[i])
	}
	return out
}
