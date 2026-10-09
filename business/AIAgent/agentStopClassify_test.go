package business

import "testing"

// classifyStop is the pure decision behind the durable worker's RunStopped
// handling. It must key off the STABLE stop-reason code (so a message wording
// change can never silently turn a budget-pause into a "done" finalize), and
// fall back to text only when no code is present.
func TestClassifyStop_ByStableCode(t *testing.T) {
	cases := []struct {
		name string
		code string
		want stopDisposition
	}{
		{"user budget pauses", StopReasonUserBudget, stopAwaitBudget},
		{"agent budget pauses", StopReasonAgentBudget, stopAwaitBudget},
		{"channel budget pauses", StopReasonChannelBudget, stopAwaitBudget},
		{"workspace budget pauses", StopReasonWorkspaceBudget, stopAwaitBudget},
		{"circuit open retries", StopReasonCircuitOpen, stopRetryTransient},
		{"rate limited retries", StopReasonRateLimited, stopRetryTransient},
		{"run timeout retries", StopReasonRunTimeout, stopRetryTransient},
		{"step limit finalizes", StopReasonStepLimit, stopFinalizePartial},
		{"run-token limit finalizes", StopReasonRunTokenLimit, stopFinalizePartial},
		// A cancelled run that reaches HERE had no human stop on record (a stop
		// is settled before classification): the lease was lost or the process is
		// shutting down. Both must put the job back on the queue — finalizing it
		// as a bounded completion would quietly abandon unfinished work.
		{"canceled retries", StopReasonCanceled, stopRetryTransient},
		// Paused, deleted or its sponsor gone: done with what it did, never
		// retried (a retry would just stop again).
		{"agent off finalizes", StopReasonAgentOff, stopFinalizePartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The code must win regardless of any misleading error text.
			if got := classifyStop(c.code, "totally unrelated message"); got != c.want {
				t.Fatalf("classifyStop(%q) = %d, want %d", c.code, got, c.want)
			}
		})
	}
}

func TestClassifyStop_TextFallbackWhenNoCode(t *testing.T) {
	cases := []struct {
		name string
		text string
		want stopDisposition
	}{
		{"budget text pauses", "this agent's daily token budget has been reached", stopAwaitBudget},
		{"circuit text retries", "AI temporarily unavailable (circuit open)", stopRetryTransient},
		{"rate limit text retries", "rate limit reached", stopRetryTransient},
		{"run time limit text retries", "reached the run time limit", stopRetryTransient},
		{"step limit text finalizes", "reached the step limit", stopFinalizePartial},
		{"unknown text finalizes", "something we have never seen", stopFinalizePartial},
		{"empty finalizes", "", stopFinalizePartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyStop("", c.text); got != c.want {
				t.Fatalf("classifyStop(\"\", %q) = %d, want %d", c.text, got, c.want)
			}
		})
	}
}

// A blank code with budget text must pause (not finalize) so a future stop path
// that forgets to set a code still degrades to the safe, resumable behavior.
func TestClassifyStop_CodeTakesPrecedenceOverText(t *testing.T) {
	// Code says transient retry; text says budget. The code must win.
	if got := classifyStop(StopReasonRateLimited, "daily token budget reached"); got != stopRetryTransient {
		t.Fatalf("expected code to take precedence (retry), got %d", got)
	}
}

// mentionNoAnswerReply is the channel-mention analog of classifyStop: it must
// key off the stable StopReason code so an @mention is answered (or silently
// skipped during an outage) consistently regardless of message wording.
func TestMentionNoAnswerReply_ByStableCode(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		wantPost bool
		wantText string
	}{
		{"circuit stays silent", StopReasonCircuitOpen, false, ""},
		{"rate limit stays silent", StopReasonRateLimited, false, ""},
		{"a paused agent stays silent", StopReasonAgentOff, false, ""},
		{"agent budget explains pause", StopReasonAgentBudget, true, budgetPausedMentionMsg},
		{"workspace budget explains pause", StopReasonWorkspaceBudget, true, budgetPausedMentionMsg},
		{"timeout gets generic note", StopReasonRunTimeout, true, genericMentionRetryMsg},
		{"step limit gets generic note", StopReasonStepLimit, true, genericMentionRetryMsg},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, post := mentionNoAnswerReply(&RunOutcome{StopReason: c.code, Error: "unrelated text"})
			if post != c.wantPost || got != c.wantText {
				t.Fatalf("mentionNoAnswerReply(%q) = (%q,%v), want (%q,%v)", c.code, got, post, c.wantText, c.wantPost)
			}
		})
	}
}

func TestMentionNoAnswerReply_TextFallback(t *testing.T) {
	// Blank code: fall back to text. A transient stop stays silent; budget text
	// explains; unknown text gets the generic note.
	if got, post := mentionNoAnswerReply(&RunOutcome{Error: "rate limit reached"}); post || got != "" {
		t.Fatalf("transient text should stay silent, got (%q,%v)", got, post)
	}
	if got, post := mentionNoAnswerReply(&RunOutcome{Error: "daily token budget reached"}); !post || got != budgetPausedMentionMsg {
		t.Fatalf("budget text should explain pause, got (%q,%v)", got, post)
	}
	if got, post := mentionNoAnswerReply(&RunOutcome{Error: "weird unseen error"}); !post || got != genericMentionRetryMsg {
		t.Fatalf("unknown text should get generic note, got (%q,%v)", got, post)
	}
}
