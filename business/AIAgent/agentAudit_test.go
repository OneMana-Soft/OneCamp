package business

import (
	"strings"
	"testing"
)

// The one line a reviewer reads. It must say what executed, not what the model
// said it did: this package already carries verifyWorkHappened precisely
// because a run can narrate work no tool performed.
func TestAuditRunSummary(t *testing.T) {
	cases := []struct {
		name     string
		agent    string
		status   string
		tools    []string
		dryRun   bool
		contains []string
	}{
		{
			name:  "a run that changed something names what it used",
			agent: "Triage", status: "completed", tools: []string{"create_task", "post_message"},
			contains: []string{"Triage", "completed", "create_task", "post_message"},
		},
		{
			// The distinction that matters most on a review screen: a run can
			// finish perfectly well having touched nothing.
			name:  "a run that touched nothing says so",
			agent: "Triage", status: "completed",
			contains: []string{"changed nothing"},
		},
		{
			name:  "a dry run is not reported as work",
			agent: "Triage", status: "completed", tools: []string{"create_task"}, dryRun: true,
			contains: []string{"dry run", "proposing rather than writing"},
		},
		{
			name:  "an unnamed agent still reads as a sentence",
			agent: "   ", status: "failed",
			contains: []string{"an agent", "failed"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := auditRunSummary(c.agent, c.status, c.tools, c.dryRun)
			for _, want := range c.contains {
				if !strings.Contains(got, want) {
					t.Errorf("summary %q does not contain %q", got, want)
				}
			}
		})
	}
}

// A dry run must never be summarised as though it wrote, because the difference
// is the entire point of offering one.
func TestADryRunIsNeverSummarisedAsWork(t *testing.T) {
	got := auditRunSummary("Triage", "completed", []string{"delete_task"}, true)
	if strings.Contains(got, "using delete_task") {
		t.Fatalf("a dry run is reported as having used a destructive tool: %s", got)
	}
}
