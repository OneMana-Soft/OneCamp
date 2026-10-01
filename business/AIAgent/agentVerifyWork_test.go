package business

import "testing"

// The question this answers: an agent is asked to do something, does not do it,
// and says it did. What stops that reaching the person who asked?
//
// verifyRunClaims covers the case where a write was attempted and errored.
// verifyWorkHappened covers the one it cannot see, which is also the more
// common one: no write was attempted at all.
func TestVerifyWorkHappened(t *testing.T) {
	cases := []struct {
		name      string
		draft     string
		writes    []string
		wantFlag  bool
		reasoning string
	}{
		{
			name:      "claims a change with nothing written",
			draft:     "I've updated the onboarding doc with the new steps.",
			writes:    nil,
			wantFlag:  true,
			reasoning: "the exact failure: confident mutation claim, no write tool ran",
		},
		{
			name:     "claims a change and a write really happened",
			draft:    "I've updated the onboarding doc with the new steps.",
			writes:   []string{"update_doc"},
			wantFlag: false,
		},
		{
			name:      "read-only answer that merely reads as complete",
			draft:     "Done. There are three open tasks in the project.",
			writes:    nil,
			wantFlag:  false,
			reasoning: "answering a question IS the work; flagging this punishes correct runs",
		},
		{
			name:      "successful read reported with the word successfully",
			draft:     "I successfully found the four channels you asked about.",
			writes:    nil,
			wantFlag:  false,
			reasoning: "why the write list is narrower than the success list",
		},
		{
			name:      "already honest about not doing it",
			draft:     "I couldn't create the task because I do not have permission.",
			writes:    nil,
			wantFlag:  false,
			reasoning: "owning the failure needs no correction",
		},
		{
			name:      "describes someone else's past change",
			draft:     "The task was assigned to Sam last week and is still open.",
			writes:    nil,
			wantFlag:  false,
			reasoning: "reporting a change is not claiming to have made one",
		},
		{
			name:     "a write was attempted but only a read succeeded",
			draft:    "I've assigned it to Priya.",
			writes:   nil,
			wantFlag: true,
		},
		{
			name:     "empty draft",
			draft:    "   ",
			writes:   nil,
			wantFlag: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := verifyWorkHappened(c.draft, c.writes).NeedsCorrection
			if got != c.wantFlag {
				t.Fatalf("NeedsCorrection = %v, want %v\n  draft: %q\n  writes: %v\n  %s",
					got, c.wantFlag, c.draft, c.writes, c.reasoning)
			}
		})
	}
}

// A read tool succeeding must not count as work, which is the whole reason the
// stall guard failed to catch this class.
func TestSucceededWriteToolsExcludesReads(t *testing.T) {
	// Real registry names, because inventing tool names made this test assert
	// that unknown tools are reads, which is the opposite of the fail-safe.
	got := succeededWriteTools([]string{"list_tasks", "list_projects", "read_project"})
	if len(got) != 0 {
		t.Fatalf("reads counted as writes: %v", got)
	}
	// An unknown tool is treated as a write, matching failedWriteTools: assuming
	// an unrecognised MCP tool is read-only would let a real change go unrecorded.
	if got := succeededWriteTools([]string{"some_mcp_tool"}); len(got) != 1 {
		t.Fatalf("an unknown tool should be treated as a write, got %v", got)
	}
}
