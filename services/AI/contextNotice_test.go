package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Nothing to report must produce nothing. A notice on every answer is a notice nobody
// reads, which would waste the one chance to tell someone their context was cut.
func TestNoNoticeWhenNothingWasShortened(t *testing.T) {
	var n ContextNotice
	if n.Any() {
		t.Error("the zero value must report nothing happened")
	}
	if n.Message() != "" {
		t.Errorf("the zero value must have no message, got %q", n.Message())
	}
}

// The wording must inform without reading as a failure, and must name the lever.
func TestNoticeWordingIsAFootnoteNotAnError(t *testing.T) {
	for _, c := range []struct {
		name   string
		n      ContextNotice
		expect []string
	}{
		{"trimmed", ContextNotice{Trimmed: true}, []string{"shortened", "context window"}},
		{"rescued", ContextNotice{Rescued: true}, []string{"shortened", "smaller part", "larger context window"}},
	} {
		msg := c.n.Message()
		if msg == "" {
			t.Fatalf("%s: expected a message", c.name)
		}
		for _, want := range c.expect {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message should mention %q, got %q", c.name, want, msg)
			}
		}
		// This is a successful answer. Words that frame it as a failure would teach
		// people to distrust answers that are fine.
		for _, banned := range []string{"error", "failed", "Error", "Failed", "try again"} {
			if strings.Contains(msg, banned) {
				t.Errorf("%s: message must not read as a failure, found %q in %q", c.name, banned, msg)
			}
		}
	}
	// A rescue is the stronger fact and must win when both are set: it means the budget
	// was wrong rather than merely tight, so more was dropped than planned.
	both := ContextNotice{Trimmed: true, Rescued: true}
	if both.Message() != (ContextNotice{Rescued: true}).Message() {
		t.Error("a rescue must take precedence over a pre-emptive trim in the message")
	}
}

// The sink accumulates rather than overwrites: one answer can be trimmed on the way in
// and rescued afterwards, and losing the stronger fact would understate what happened.
func TestNoticeSinkMergesObservations(t *testing.T) {
	ctx := WithContextNoticeSink(context.Background())
	reportContextNotice(ctx, ContextNotice{Trimmed: true, DroppedMessages: 3})
	reportContextNotice(ctx, ContextNotice{Rescued: true, DroppedMessages: 2})

	got := TakeContextNotice(ctx)
	if !got.Trimmed || !got.Rescued {
		t.Errorf("both facts must survive, got %+v", got)
	}
	if got.DroppedMessages != 5 {
		t.Errorf("dropped counts must add up: got %d, want 5", got.DroppedMessages)
	}
	// Taking resets, so the next answer starts clean and cannot inherit this one's.
	if again := TakeContextNotice(ctx); again.Any() {
		t.Errorf("the sink must reset after being taken, got %+v", again)
	}
}

// Without a sink every report is a no-op, so paths that do not show a notice to anyone
// pay nothing and behave exactly as before.
func TestNoticeIsANoOpWithoutASink(t *testing.T) {
	ctx := context.Background()
	reportContextNotice(ctx, ContextNotice{Rescued: true})
	if TakeContextNotice(ctx).Any() {
		t.Error("a context with no sink must report nothing")
	}
	if TakeContextNotice(nil).Any() {
		t.Error("a nil context must be safe and report nothing")
	}
	// And the idempotent attach must not replace an existing sink, or a nested call
	// would silently start a second one and the outer read would come back empty.
	outer := WithContextNoticeSink(context.Background())
	reportContextNotice(outer, ContextNotice{Trimmed: true})
	if !TakeContextNotice(WithContextNoticeSink(outer)).Trimmed {
		t.Error("re-attaching must reuse the existing sink")
	}
}

// The reporting truncators must report only when they actually cut something.
func TestTruncateForPromptReportsOnlyRealCuts(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 8192}

	ctx := WithContextNoticeSink(context.Background())
	lim.TruncateForPrompt(ctx, "short enough", 1000)
	if TakeContextNotice(ctx).Any() {
		t.Error("text that fits must not produce a notice")
	}

	ctx = WithContextNoticeSink(context.Background())
	out := lim.TruncateForPrompt(ctx, strings.Repeat("a long transcript. ", 2000), 200)
	if !TakeContextNotice(ctx).Trimmed {
		t.Error("text that was cut must produce a notice")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("the model must still see the truncation marker")
	}
}

// Dropped history turns are counted, so the notice can be specific rather than vague.
func TestTrimHistoryForPromptCountsDroppedTurns(t *testing.T) {
	lim := ModelLimits{Provider: "p", Model: "m", ContextWindow: 8192}
	var history []ChatMessage
	for i := 0; i < 30; i++ {
		history = append(history, ChatMessage{Role: roleUser, Content: strings.Repeat("turn. ", 30)})
	}

	ctx := WithContextNoticeSink(context.Background())
	kept := lim.TrimHistoryForPrompt(ctx, history, 300)
	n := TakeContextNotice(ctx)
	if !n.Trimmed {
		t.Fatal("dropping turns must produce a notice")
	}
	if n.DroppedMessages != len(history)-len(kept) {
		t.Errorf("dropped count %d does not match %d turns actually removed", n.DroppedMessages, len(history)-len(kept))
	}

	// Everything fitting must stay silent.
	ctx = WithContextNoticeSink(context.Background())
	lim.TrimHistoryForPrompt(ctx, history[:2], 100000)
	if TakeContextNotice(ctx).Any() {
		t.Error("history that fits must not produce a notice")
	}
}

// A successful rescue is reported; a rescue that still failed is not.
//
// The second half matters: the caller gets an error in that case, and telling someone
// their answer was shortened when they did not receive an answer would be nonsense.
func TestRescueReportsOnlyWhenItSucceeded(t *testing.T) {
	lim := ModelLimits{Provider: "openai_compatible", Model: "m", ContextWindow: 8192}

	ctx := WithContextNoticeSink(WithModelLimits(context.Background(), lim))
	if _, err := ChatWithRescue(ctx, &overflowingProvider{limit: 3000, lim: lim}, longConversation(lim), ChatOptions{MaxTokens: 512}); err != nil {
		t.Fatalf("precondition: the rescue should have succeeded, got %v", err)
	}
	if n := TakeContextNotice(ctx); !n.Rescued {
		t.Errorf("a successful rescue must be reported, got %+v", n)
	}

	// The case that actually exercises the success check: the shrink WORKS (turns are
	// dropped) but the provider still refuses, because its real limit is far below
	// anything the prompt can reach. The rescue ran, so a report placed before the error
	// check would fire here — and must not, since the caller is returning an error.
	ctx = WithContextNoticeSink(WithModelLimits(context.Background(), lim))
	stubborn := &overflowingProvider{limit: 40, lim: lim}
	if _, err := ChatWithRescue(ctx, stubborn, longConversation(lim), ChatOptions{MaxTokens: 512}); err == nil {
		t.Fatal("precondition: a 40-token limit cannot be satisfied")
	}
	if len(stubborn.attempts) < 2 {
		t.Fatalf("precondition: the shrink must have produced retries, saw %d attempt(s)", len(stubborn.attempts))
	}
	if n := TakeContextNotice(ctx); n.Any() {
		t.Errorf("a rescue that still failed must not claim the answer was shortened, got %+v", n)
	}

	// A prompt nothing can shrink at all: fails immediately, still silent.
	ctx = WithContextNoticeSink(WithModelLimits(context.Background(), lim))
	unshrinkable := []ChatMessage{{Role: roleSystem, Content: strings.Repeat("rules. ", 5000)}}
	if _, err := ChatWithRescue(ctx, &overflowingProvider{limit: 10, lim: lim}, unshrinkable, ChatOptions{}); err == nil {
		t.Fatal("precondition: this prompt cannot be rescued")
	}
	if n := TakeContextNotice(ctx); n.Any() {
		t.Errorf("an unshrinkable prompt must not claim the answer was shortened, got %+v", n)
	}
}

// Guards the boundary between the two mechanisms: a notice is about a SUCCESSFUL answer,
// FriendlyProviderError is about a failed one. They must never describe the same event.
func TestNoticeAndErrorMessageAreDistinct(t *testing.T) {
	notice := ContextNotice{Rescued: true}.Message()
	errMsg := FriendlyProviderError(errors.New("status 400: context_length_exceeded"))
	if notice == errMsg {
		t.Error("the success footnote and the failure message must not be the same text")
	}
	if strings.Contains(notice, "even after trimming") {
		t.Error("the footnote must not carry the failure message's wording")
	}
}

// The footnote is for surfaces whose response IS a posted message — the in-channel
// coworker, DM and group replies — which have no envelope to carry a field.
func TestAppendNoticeFootnote(t *testing.T) {
	const answer = "The launch is on Thursday."

	// Nothing to report leaves the reply byte-identical, so a normal answer never grows
	// a footnote.
	if got := AppendNoticeFootnote(answer, ContextNotice{}); got != answer {
		t.Errorf("an empty notice must not alter the reply, got %q", got)
	}

	got := AppendNoticeFootnote(answer, ContextNotice{Trimmed: true})
	if !strings.HasPrefix(got, answer) {
		t.Error("the answer must come first and survive unchanged")
	}
	if !strings.Contains(got, "_") {
		t.Error("the footnote should be italic so it reads as an aside")
	}
	if !strings.Contains(got, "\n\n") {
		t.Error("the footnote must be separated from the answer, not run into it")
	}

	// An empty reply gets no footnote: there is no answer for it to describe.
	for _, empty := range []string{"", "   ", "\n\n"} {
		if got := AppendNoticeFootnote(empty, ContextNotice{Rescued: true}); got != empty {
			t.Errorf("an empty reply must not gain a footnote, got %q", got)
		}
	}

	// Trailing whitespace is normalised so the separation is exactly one blank line
	// rather than however many the model happened to emit.
	if got := AppendNoticeFootnote(answer+"\n\n\n", ContextNotice{Trimmed: true}); strings.Contains(got, "\n\n\n") {
		t.Errorf("trailing blank lines must be collapsed, got %q", got)
	}
}
