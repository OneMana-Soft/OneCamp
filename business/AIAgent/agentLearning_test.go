package business

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func transcript(t *testing.T, steps []stepRecord) string {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func run(status, prompt, steps string, at time.Time) *model.AgentRun {
	return &model.AgentRun{
		Id: uuid.New(), Status: status, TriggerPrompt: prompt,
		Steps: steps, StartedAt: at,
	}
}

// A run with no prompt cannot become a scenario, and must not become a
// half-invented one. Runs recorded before the prompt was kept are exactly this.
func TestARunWithoutItsPromptProposesNothing(t *testing.T) {
	r := run(model.RunFailed, "   ", "", time.Now())
	if _, ok := proposeScenario(r, nil); ok {
		t.Fatal("proposed a scenario with no prompt to run")
	}
}

// A successful run is not evidence of anything that needs catching.
func TestASuccessfulRunProposesNothing(t *testing.T) {
	r := run(model.RunSucceeded, "summarise the channel", "", time.Now())
	if _, ok := proposeScenario(r, nil); ok {
		t.Fatal("proposed a test for a run that worked")
	}
}

// The assertion must come from what was observed. A failed tool is the sharpest
// case: the test is that the tool works.
func TestAFailedToolBecomesTheAssertion(t *testing.T) {
	r := run(model.RunFailed, "create the release task", "", time.Now())
	p, ok := proposeScenario(r, []string{"create_task"})
	if !ok {
		t.Fatal("no proposal")
	}
	if len(p.Expectations.ExpectedTools) != 1 || p.Expectations.ExpectedTools[0] != "create_task" {
		t.Fatalf("expectations do not name the failed tool: %+v", p.Expectations)
	}
	if p.Expectations.ExpectedStatus != model.RunSucceeded {
		t.Fatalf("status expectation = %q", p.Expectations.ExpectedStatus)
	}
	if !strings.Contains(p.Why, "create_task") {
		t.Fatalf("why does not name the tool: %q", p.Why)
	}
	if p.Prompt != "create the release task" {
		t.Fatalf("prompt not carried: %q", p.Prompt)
	}
}

func TestAStoppedRunProposesThatItShouldFinish(t *testing.T) {
	p, ok := proposeScenario(run(model.RunStopped, "do the thing", "", time.Now()), nil)
	if !ok || p.Expectations.ExpectedStatus != model.RunSucceeded {
		t.Fatalf("stopped run did not propose completion: %+v", p)
	}
}

// One run that retried a tool nine times is ONE problem. Counting occurrences
// rather than runs would report it as nine and send somebody chasing a phantom.
func TestOneRunCountsOnceHoweverManyTimesItRetried(t *testing.T) {
	nine := make([]stepRecord, 0, 9)
	for i := 0; i < 9; i++ {
		nine = append(nine, stepRecord{
			Iteration: i,
			ToolCalls: []toolCallRecord{{Tool: "flaky_tool", Error: "boom"}},
		})
	}
	r := run(model.RunFailed, "p", transcript(t, nine), time.Now())

	if got := failedToolsIn(parseSteps(r.Steps)); len(got) != 1 {
		t.Fatalf("expected one distinct failed tool, got %v", got)
	}
	pats := patternsFrom([]*model.AgentRun{r}, map[uuid.UUID][]stepRecord{r.Id: parseSteps(r.Steps)})
	if len(pats) != 0 {
		t.Fatalf("one run should not reach the recurrence threshold, got %+v", pats)
	}
}

// The threshold is the whole point: an incident is not a habit.
func TestAPatternNeedsToRecurBeforeItIsRaised(t *testing.T) {
	steps := transcript(t, []stepRecord{{ToolCalls: []toolCallRecord{{Tool: "send_email", Error: "smtp"}}}})
	now := time.Now()

	mk := func(n int) ([]*model.AgentRun, map[uuid.UUID][]stepRecord) {
		var runs []*model.AgentRun
		m := map[uuid.UUID][]stepRecord{}
		for i := 0; i < n; i++ {
			r := run(model.RunFailed, "p", steps, now.Add(-time.Duration(i)*time.Hour))
			runs = append(runs, r)
			m[r.Id] = parseSteps(steps)
		}
		return runs, m
	}

	runs, m := mk(minRecurrenceForSkillProposal - 1)
	if got := patternsFrom(runs, m); len(got) != 0 {
		t.Fatalf("raised a pattern below the threshold: %+v", got)
	}

	runs, m = mk(minRecurrenceForSkillProposal)
	got := patternsFrom(runs, m)
	if len(got) != 1 || got[0].Subject != "send_email" || got[0].Count != minRecurrenceForSkillProposal {
		t.Fatalf("pattern not raised at the threshold: %+v", got)
	}
	if !strings.Contains(got[0].Suggestion, "skill") {
		t.Fatalf("suggestion names no lever: %q", got[0].Suggestion)
	}
}

// Approval-required is the governance system working as configured. Reporting
// it as a failure would train an operator to ignore the whole screen.
func TestApprovalRequiredIsNotAFailure(t *testing.T) {
	steps := parseSteps(transcript(t, []stepRecord{{ToolCalls: []toolCallRecord{
		{Tool: "delete_task", Governance: govApprovalRequired},
		{Tool: "drop_table", Governance: govBlocked},
	}}}))
	got := governanceBlocksIn(steps)
	if len(got) != 1 || !strings.Contains(got[0], "drop_table") {
		t.Fatalf("expected only the outright block, got %v", got)
	}
}

// A block subject must name the capability, not just say "blocked", or nobody
// can act on it.
func TestABlockNamesTheToolItRefused(t *testing.T) {
	steps := parseSteps(transcript(t, []stepRecord{{ToolCalls: []toolCallRecord{
		{Tool: "delete_channel", Governance: govBlocked},
	}}}))
	got := governanceBlocksIn(steps)
	if len(got) != 1 || !strings.Contains(got[0], "delete_channel") {
		t.Fatalf("block subject is not actionable: %v", got)
	}
}

// A malformed transcript must cost only its own run.
func TestAnUnreadableTranscriptIsSkippedNotFatal(t *testing.T) {
	if got := parseSteps("{not json"); got != nil {
		t.Fatalf("expected nothing from a malformed transcript, got %+v", got)
	}
	if got := failedToolsIn(parseSteps("")); len(got) != 0 {
		t.Fatalf("expected nothing from an empty transcript, got %v", got)
	}
}

// Most frequent first, because that is the order somebody fixes them in.
func TestPatternsAreOrderedByFrequency(t *testing.T) {
	now := time.Now()
	var runs []*model.AgentRun
	m := map[uuid.UUID][]stepRecord{}
	add := func(tool string, n int) {
		st := transcript(t, []stepRecord{{ToolCalls: []toolCallRecord{{Tool: tool, Error: "e"}}}})
		for i := 0; i < n; i++ {
			r := run(model.RunFailed, "p", st, now)
			runs = append(runs, r)
			m[r.Id] = parseSteps(st)
		}
	}
	add("rare_tool", 3)
	add("common_tool", 6)

	got := patternsFrom(runs, m)
	if len(got) != 2 || got[0].Subject != "common_tool" {
		t.Fatalf("not ordered by frequency: %+v", got)
	}
}
