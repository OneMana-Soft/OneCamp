package business

import (
	"regexp"
	"strings"
)

// Stall/placeholder answer detection.
//
// A weak model sometimes NARRATES what it would do — or fabricates a templated
// answer with placeholders — instead of emitting a tool call to do the actual
// work. Example (observed in production):
//
//	"I will first search for the repository and then search for commits…
//	 Please wait for the tool results… Assuming the tool results are available,
//	 here is a concise reply: 'We shipped … [list of commits].'"
//
// Posting that as the agent's answer is misleading: no tool ran, and the
// "result" is a placeholder. The runner uses looksLikeNonAnswer to catch such a
// turn and nudge the model to actually act (or fail cleanly), rather than
// accepting it as a real answer. This is fully model-agnostic — it inspects the
// text, not any provider-specific field.

const (
	// maxStallRetries bounds how many times, within a single run, we re-prompt
	// a model that produced a plan/placeholder answer with no tool call. One
	// corrective nudge is enough for a model that merely needed a firmer push;
	// beyond that we stop rather than loop.
	maxStallRetries = 1

	// stallCorrection is the corrective user turn injected when the model
	// narrated a plan / emitted placeholders without calling any tool. It is
	// deliberately concrete about the failure so even a weak model course-
	// corrects to an actual tool call or an honest answer.
	stallCorrection = "You did not use any tool, and your reply only describes what you WOULD do or contains " +
		"placeholders (e.g. \"please wait\", \"assuming the tool results\", \"[list of …]\"). Do the work NOW: if you " +
		"need data, emit a <tool_call> to fetch it and then answer with the ACTUAL results. Never reply with a plan " +
		"only, and never write placeholder text. If no tool can help and you cannot answer, emit the needs_human tool call."
)

// stallPhrases are lowercase substrings that signal a plan-narration or a
// stalling promise rather than a delivered answer. Kept specific (not generic
// future tense) so a legitimate answer that merely says "I will help" is not
// flagged.
var stallPhrases = []string{
	"assuming the tool results",
	"assuming the results",
	"once i have the results",
	"once the tool",
	"once the results",
	"please wait for the tool",
	"wait for the tool results",
	"waiting for the tool",
	"not actually executed",
	"is not actually executed",
	"tool call is not actually",
	"provide a general response",
	"i can only provide a general",
	"i'll get back to you",
	"i will get back to you",
	"get back to you with",
	"i'll search and reply",
	"i will search and reply",
	"i'll look into it and",
	"here is a concise reply:",
	"here's a concise reply:",
	"here is the concise reply:",
}

// placeholderBracketRe matches a bracketed placeholder token such as
// "[list of commits]", "[insert summary]", "[your answer here]" — a strong sign
// the model templated a fake result instead of producing one. Requires a
// letter-led phrase of a few chars so real bracketed content like "[1]" or an
// emoji list is not caught.
var placeholderBracketRe = regexp.MustCompile(`\[[a-zA-Z][a-zA-Z ._/-]{3,}\]`)

// looksLikeNonAnswer reports whether a tool-call-free assistant turn reads as a
// plan narration / stalling promise / placeholder template rather than a real
// answer. Pure and DB-free for unit testing. Empty text is NOT treated as a
// non-answer here (the empty-result path is handled separately by the runner's
// forceSummarize + mention fallbacks).
func looksLikeNonAnswer(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	for _, p := range stallPhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return placeholderBracketRe.MatchString(text)
}

// A deflection: a short reply that does nothing and hands the work back.
//
// Seen on the demo after the model had used a tool, so the stall guard above
// (which fires only when no tool succeeded) let it through: a delegated "add
// the rollback steps to the Launch sync notes" was answered "I'm ready for the
// next step. Let me know what you'd like to do next.", and another "No new
// action is required at this time." with nothing about what was checked. Both
// were recorded as done work.
//
// Only SHORT replies count, so "let me know if you need anything else" at the
// end of a real answer is untouched. One nudge, never more.
const (
	maxDeflectionRetries = 1
	deflectionMaxRunes   = 200

	deflectionCorrection = "Your reply hands the work back without doing it or saying what you found. " +
		"Do what was asked now with your tools. Where a change needs approval it waits for it on its own, so make the change rather than asking whether to. " +
		"If it turns out nothing needs doing, say concretely what you checked and what you found there. " +
		"If you cannot continue without a person, emit the needs_human tool call with your question."
)

var deflectionPhrases = []string{
	"ready for the next step",
	"ready for your next instruction",
	"ready for the next instruction",
	"let me know what you'd like",
	"let me know what you would like",
	"let me know how you'd like to proceed",
	"let me know how you would like to proceed",
	"what would you like me to do",
	"how can i help",
	"how can i assist",
	"no new action is required",
	"no action is required at this time",
	"no further action is needed",
}

// looksLikeDeflection reports whether a final reply is only a hand-back. Pure.
func looksLikeDeflection(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" || len([]rune(s)) > deflectionMaxRunes {
		return false
	}
	s = strings.ReplaceAll(s, "’", "'")
	for _, p := range deflectionPhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// offerLeadIns open a reply that hands the choice of what to do back to the
// person ("Would you like me to: - update it, - add a task ...?").
var offerLeadIns = []string{
	"would you like me to",
	"would you like me ",
	"do you want me to",
	"want me to ",
	"shall i ",
	"should i ",
	"if you'd like, i can",
	"if you would like, i can",
	"let me know if you'd like",
	"let me know if you want",
}

// offerTailRunes is how much of the end of a reply is read for an offer: the
// question plus the short list of options that usually follows it.
const offerTailRunes = 600

// endsWithOffer reports whether a reply ends by asking the person which thing
// to do next. Pure.
func endsWithOffer(text string) bool {
	r := []rune(strings.ToLower(strings.TrimSpace(text)))
	if len(r) > offerTailRunes {
		r = r[len(r)-offerTailRunes:]
	}
	tail := strings.ReplaceAll(string(r), "’", "'")
	if !strings.Contains(tail, "?") {
		return false
	}
	for _, p := range offerLeadIns {
		if strings.Contains(tail, p) {
			return true
		}
	}
	return false
}

// handsBackWork reports whether a final reply hands the work back instead of
// doing it: a short stock hand-back anywhere, or, in delegated work that has
// not attempted a single change, a reply that ends by offering choices. The
// second is what an assigned task got on the demo, and a bare acknowledgement
// ("Got it.") is its twin ("Would you like me to:
// update the status, add a task, or provide a plan?") after reading everything
// it needed to do what the task plainly said. In a conversation, offering
// options is a fine way to end, so only delegated work is held to it. Pure.
func handsBackWork(text string, delegated, attemptedChange bool) bool {
	if looksLikeDeflection(text) {
		return true
	}
	if !delegated || attemptedChange {
		return false
	}
	// "Got it." after reading everything: an acknowledgement is not a report.
	return endsWithOffer(text) || len([]rune(strings.TrimSpace(text))) < bareReplyRunes
}

// bareReplyRunes is below any report of what was done or found.
const bareReplyRunes = 40

// repeatedCallObservation is what a model is told when it repeats a call that
// already returned in this run: the earlier result again, and that asking a
// third time will not produce anything new. Pure.
func repeatedCallObservation(prior string) string {
	return "you already made this exact call in this run, so it was not run again. Its result was:\n" +
		truncateObservation(prior) +
		"\nUse this result. Do not repeat the call: act on it, try a different call, or finish."
}
