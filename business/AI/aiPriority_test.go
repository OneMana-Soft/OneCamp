package business

import "testing"

// UX-PROJECTS-H1-BE (PROJECTS-14): a task an assistant makes (create_task, the
// action items it finds) has the priority its words name, and none when they
// name none or none of the priorities. Before, both were medium, so every
// task an assistant made read as medium.
func TestAnAssistantsTaskHasNoPriorityUnlessItsWordsNameOne(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "  ": "", "banana": "", "none": "", "No priority": "",
		"P1": "high", "urgent": "urgent", "normal": "medium", "Low": "low",
	} {
		if got := normalizeTaskPriority(in); got != want {
			t.Errorf("normalizeTaskPriority(%q) = %q, want %q", in, got, want)
		}
	}
}
