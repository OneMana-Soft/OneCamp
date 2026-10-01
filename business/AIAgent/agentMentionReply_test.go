package business

import (
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestAgentMentionReply(t *testing.T) {
	// A real answer is posted verbatim.
	if got, ok := agentMentionReply(&RunOutcome{Status: model.RunSucceeded, Result: "Here you go"}); !ok || got != "Here you go" {
		t.Fatalf("expected the answer to be posted, got (%q, %v)", got, ok)
	}

	// Nil outcome posts nothing.
	if _, ok := agentMentionReply(nil); ok {
		t.Fatal("nil outcome must not post")
	}

	// Transient throttle/circuit stops stay silent (no reply storm).
	for _, reason := range []string{
		"AI temporarily unavailable (circuit open)",
		"rate limit reached",
	} {
		if _, ok := agentMentionReply(&RunOutcome{Status: model.RunStopped, Error: reason}); ok {
			t.Fatalf("transient stop %q must stay silent", reason)
		}
	}

	// Token-budget stop posts an explainable pause message.
	got, ok := agentMentionReply(&RunOutcome{Status: model.RunStopped, Error: "the workspace AI token budget for today has been reached"})
	if !ok || !strings.Contains(strings.ToLower(got), "usage limit") {
		t.Fatalf("budget stop should post a pause message, got (%q, %v)", got, ok)
	}

	// Any other empty-result outcome (failure / empty success / step limit)
	// posts a brief honest note rather than staying silent (Req 6.2).
	for _, oc := range []*RunOutcome{
		{Status: model.RunFailed, Error: "the AI model call failed"},
		{Status: model.RunSucceeded, Result: "   "},
		{Status: model.RunStopped, Error: "reached the step limit"},
	} {
		if got, ok := agentMentionReply(oc); !ok || strings.TrimSpace(got) == "" {
			t.Fatalf("expected a fallback note for %+v, got (%q, %v)", oc, got, ok)
		}
	}

	// Approval-mode proposals are disclosed in the channel deterministically,
	// appended to the answer body.
	got, ok = agentMentionReply(&RunOutcome{
		Status:   model.RunSucceeded,
		Result:   "On it.",
		Proposed: []string{"Create task \"Fix login\""},
	})
	if !ok || !strings.Contains(got, "On it.") || !strings.Contains(strings.ToLower(got), "approval") {
		t.Fatalf("expected the answer plus an approval disclosure, got (%q, %v)", got, ok)
	}

	// A run that proposed changes but produced no summary still discloses the
	// proposals rather than a generic fallback note.
	got, ok = agentMentionReply(&RunOutcome{
		Status:   model.RunSucceeded,
		Proposed: []string{"Update the channel topic", "Archive #old"},
	})
	if !ok || !strings.Contains(got, "2 changes") || !strings.Contains(strings.ToLower(got), "approval") {
		t.Fatalf("expected an approval disclosure for a body-less proposal run, got (%q, %v)", got, ok)
	}
}

func TestProposedApprovalFooter(t *testing.T) {
	if proposedApprovalFooter(nil) != "" {
		t.Fatal("no proposals should yield no footer")
	}
	if proposedApprovalFooter([]string{"   ", ""}) != "" {
		t.Fatal("blank-only proposals should yield no footer")
	}
	// Singular phrasing.
	one := proposedApprovalFooter([]string{"Create a task"})
	if !strings.Contains(one, "1 change") || !strings.Contains(one, "Create a task") {
		t.Fatalf("unexpected singular footer: %q", one)
	}
	// Plural count + cap at 4 with an overflow summary.
	many := proposedApprovalFooter([]string{"a", "b", "c", "d", "e", "f"})
	if !strings.Contains(many, "6 changes") {
		t.Fatalf("expected the full count, got %q", many)
	}
	if !strings.Contains(many, "and 2 more") {
		t.Fatalf("expected an overflow summary, got %q", many)
	}
}

func TestScheduledCheckinText(t *testing.T) {
	// A real update is posted.
	if got, ok := scheduledCheckinText(&RunOutcome{Status: model.RunSucceeded, Result: "3 PRs merged today."}); !ok || got != "3 PRs merged today." {
		t.Fatalf("expected the update to post, got (%q, %v)", got, ok)
	}
	// Nil / empty / failed runs post nothing (no recurring failure noise).
	for _, oc := range []*RunOutcome{
		nil,
		{Status: model.RunSucceeded, Result: "   "},
		{Status: model.RunFailed, Error: "the AI model call failed"},
	} {
		if _, ok := scheduledCheckinText(oc); ok {
			t.Fatalf("expected no post for %+v", oc)
		}
	}
	// The "nothing to report" sentinel keeps the schedule quiet.
	for _, res := range []string{"NOTHING_TO_REPORT", "nothing_to_report", "NOTHING_TO_REPORT."} {
		if _, ok := scheduledCheckinText(&RunOutcome{Status: model.RunSucceeded, Result: res}); ok {
			t.Fatalf("sentinel %q must stay silent", res)
		}
	}
}
