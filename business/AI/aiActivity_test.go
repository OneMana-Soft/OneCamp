package business

import (
	"testing"
	"time"

	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

func TestMergeAIActivity_SortAndCap(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	runs := []AIActivityItem{
		{Kind: "agent_run", Title: "A", At: t0.Add(2 * time.Minute)},
		{Kind: "agent_run", Title: "B", At: t0},
	}
	audits := []AIActivityItem{
		{Kind: "audit", Title: "X", At: t0.Add(1 * time.Minute)},
	}
	merged := mergeAIActivity(runs, audits, 10)
	if len(merged) != 3 {
		t.Fatalf("expected 3, got %d", len(merged))
	}
	// Newest-first: A (t0+2), X (t0+1), B (t0).
	if merged[0].Title != "A" || merged[1].Title != "X" || merged[2].Title != "B" {
		t.Fatalf("wrong order: %s,%s,%s", merged[0].Title, merged[1].Title, merged[2].Title)
	}
	// Cap.
	if capped := mergeAIActivity(runs, audits, 2); len(capped) != 2 {
		t.Fatalf("expected cap of 2, got %d", len(capped))
	}
}

func TestRunsToActivity_Mapping(t *testing.T) {
	agentID := uuid.New()
	runID := uuid.New()
	errMsg := "boom happened"
	result := "did the thing"
	runs := []*agentModel.AgentRunActivity{
		{
			AgentRun:  agentModel.AgentRun{Id: runID, AgentId: agentID, Status: "failed", TriggerSource: "schedule", Error: &errMsg, StartedAt: time.Now()},
			AgentName: "Standup Bot",
		},
		{
			AgentRun:  agentModel.AgentRun{Id: uuid.New(), AgentId: agentID, Status: "succeeded", TriggerSource: "mention", Result: &result, StartedAt: time.Now()},
			AgentName: "Helper",
		},
		nil, // skipped
	}
	items := runsToActivity(runs)
	if len(items) != 2 {
		t.Fatalf("expected 2 items (nil skipped), got %d", len(items))
	}
	if items[0].Kind != "agent_run" || items[0].Status != "failed" || items[0].AgentID != agentID.String() {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	if items[0].Summary == "" || items[0].Summary[:7] != "Failed:" {
		t.Fatalf("failed run should summarize the error, got %q", items[0].Summary)
	}
	if items[1].Status != "succeeded" || items[1].Summary != "did the thing" {
		t.Fatalf("unexpected second item: %+v", items[1])
	}
}

func TestAuditsToActivity_Mapping(t *testing.T) {
	entries := []*auditModel.AuditEntry{
		{Action: "ai.web_search", Category: "security", Summary: "AI web search via tavily returned 5 results", ActorEmail: "a@x.com", CreatedAt: time.Now()},
		nil,
	}
	items := auditsToActivity(entries)
	if len(items) != 1 {
		t.Fatalf("expected 1 (nil skipped), got %d", len(items))
	}
	if items[0].Kind != "audit" || items[0].Title != "ai.web_search" || items[0].Actor != "a@x.com" {
		t.Fatalf("unexpected audit item: %+v", items[0])
	}
}

func TestNormalizeRunStatus(t *testing.T) {
	cases := map[string]string{
		"success": "succeeded", "completed": "succeeded",
		"error": "failed", "FAILED": "failed",
		"in_progress": "running", "running": "running",
		"weird": "weird",
	}
	for in, want := range cases {
		if got := normalizeRunStatus(in); got != want {
			t.Errorf("normalizeRunStatus(%q)=%q want %q", in, got, want)
		}
	}
}

// A run whose result carries a chart is summarised by naming the chart.
func TestAgentRunSummaryNamesAChart(t *testing.T) {
	res := "Here is the trend.\n```chart\n{\"type\":\"line\",\"title\":\"Trend\",\"labels\":[\"a\"],\"series\":[{\"name\":\"n\",\"values\":[1]}]}\n```"
	r := &agentModel.AgentRunActivity{}
	r.Result = &res
	got := agentRunSummary(r)
	if got != "Here is the trend.\n[chart: Trend]" {
		t.Fatalf("summary = %q", got)
	}
}
