package business

// Closing the loop: turning what happened into what is checked.
//
// Everything needed to learn from production was already recorded. A run keeps
// its transcript, the tools it used, the tools that failed, why governance
// blocked it, and now the prompt it was given. Evaluation scenarios score a run
// deterministically. Skills carry revisions and mark their agents for
// re-evaluation when edited.
//
// Nothing read any of it. Scenarios were hand-authored, skills changed only when
// a person opened the editor, and the run history fed a screen people look at
// rather than anything that acts. The experience was captured and never used,
// which is the exact gap the self-improving-agent literature says most systems
// have; the unusual thing here is having the store and not the reader.
//
// NOTHING IS APPLIED AUTOMATICALLY, and that is a product decision rather than a
// missing feature. This is a workspace whose case is that an agent's behaviour is
// governed and reviewable. A system that silently rewrote its own instructions
// would be a worse version of the thing it competes with. So the loop produces
// PROPOSALS with the evidence that motivated them, and a person decides.
//
// It is also entirely deterministic. No model drafts these. A proposal is a
// prompt that actually ran, a failure that actually happened, and a count.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

const (
	// learningWindow is how far back a proposal may look. Long enough for a
	// weekly agent to show a pattern, short enough that a fault fixed a month
	// ago stops being suggested.
	learningWindow = 30 * 24 * time.Hour

	// minRecurrenceForSkillProposal is how many times a failure must repeat
	// before it is worth an operator's attention. Once is an incident; the point
	// of this is the difference between an incident and a habit.
	minRecurrenceForSkillProposal = 3

	// maxProposals bounds a review screen. A list nobody finishes reading is a
	// list nobody acts on.
	maxProposals = 20
)

// ScenarioProposal is a run that went wrong, offered as a test that would catch
// it. Accepting one creates an ordinary eval scenario.
type ScenarioProposal struct {
	RunID uuid.UUID `json:"run_id"`
	// Name and Prompt are what the scenario would be created with.
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Expectations is what this run failed to do, stated as the assertion that
	// would have caught it.
	Expectations Expectations `json:"expectations"`
	// Why is the one line explaining the proposal, from the run itself.
	Why string `json:"why"`
	// Status and RanAt let a reviewer judge whether it is still relevant.
	Status string    `json:"status"`
	RanAt  time.Time `json:"ran_at"`
}

// FailurePattern is the same failure seen repeatedly. It proposes attention
// rather than prose: the operator writes the instruction, because inventing
// skill text from a count is exactly the kind of confident guess this product
// exists not to make.
type FailurePattern struct {
	// Kind is what recurred: "tool" for a tool that keeps failing, "block" for
	// governance refusing the same thing.
	Kind    string      `json:"kind"`
	Subject string      `json:"subject"` // the tool name, or the block reason
	Count   int         `json:"count"`
	RunIDs  []uuid.UUID `json:"run_ids"`
	FirstAt time.Time   `json:"first_at"`
	LastAt  time.Time   `json:"last_at"`
	// Suggestion names the lever, not the wording.
	Suggestion string `json:"suggestion"`
}

// LearningReview is everything the loop has to say about one agent.
type LearningReview struct {
	AgentID   uuid.UUID          `json:"agent_id"`
	Scenarios []ScenarioProposal `json:"scenario_proposals"`
	Patterns  []FailurePattern   `json:"failure_patterns"`
	// RunsConsidered and RunsWithoutPrompt explain the sample, so an empty
	// review is distinguishable from a review of nothing.
	RunsConsidered    int `json:"runs_considered"`
	RunsWithoutPrompt int `json:"runs_without_prompt"`
}

// proposeScenario turns one bad run into the scenario that would catch it.
//
// The expectations come from what the run actually did, never from a guess: a
// run that failed asserts it should succeed, a run whose tool errored asserts
// that tool must work, a run governance blocked asserts the block must not
// recur. An assertion nobody can trace to an observation is one nobody trusts.
func proposeScenario(r *model.AgentRun, failedTools []string) (ScenarioProposal, bool) {
	prompt := strings.TrimSpace(r.TriggerPrompt)
	if prompt == "" {
		return ScenarioProposal{}, false
	}

	p := ScenarioProposal{
		RunID:  r.Id,
		Prompt: prompt,
		Status: r.Status,
		RanAt:  r.StartedAt,
		Name:   scenarioNameFor(prompt, r.StartedAt),
	}

	switch {
	case len(failedTools) > 0:
		// The sharpest case: a specific tool erred, so the test is that it works.
		p.Expectations = Expectations{
			ExpectedTools:  failedTools,
			ExpectedStatus: model.RunSucceeded,
		}
		p.Why = fmt.Sprintf("%s failed during this run", strings.Join(failedTools, ", "))
	case r.Status == model.RunFailed:
		p.Expectations = Expectations{ExpectedStatus: model.RunSucceeded}
		p.Why = "the run failed"
		if r.Error != nil && strings.TrimSpace(*r.Error) != "" {
			p.Why = "the run failed: " + trimOneLineActivity(*r.Error, 120)
		}
	case r.Status == model.RunStopped:
		p.Expectations = Expectations{ExpectedStatus: model.RunSucceeded}
		p.Why = "the run stopped before finishing"
	default:
		// A successful run is not evidence of anything that needs catching.
		return ScenarioProposal{}, false
	}
	return p, true
}

// scenarioNameFor gives the proposal a name a person can recognise in a list.
func scenarioNameFor(prompt string, at time.Time) string {
	first := trimOneLineActivity(prompt, 60)
	if first == "" {
		first = "untitled"
	}
	return fmt.Sprintf("%s (%s)", first, at.Format("2 Jan"))
}

// patternsFrom counts what keeps going wrong across runs.
//
// Deliberately counts DISTINCT RUNS rather than occurrences: a single run that
// retried one tool nine times is one problem, and counting the retries would
// make it look like nine.
func patternsFrom(runs []*model.AgentRun, steps map[uuid.UUID][]stepRecord) []FailurePattern {
	type acc struct {
		runs          []uuid.UUID
		first, last   time.Time
		kind, subject string
	}
	seen := map[string]*acc{}

	note := func(kind, subject string, r *model.AgentRun) {
		if strings.TrimSpace(subject) == "" {
			return
		}
		key := kind + "\x00" + subject
		a := seen[key]
		if a == nil {
			a = &acc{kind: kind, subject: subject, first: r.StartedAt, last: r.StartedAt}
			seen[key] = a
		}
		a.runs = append(a.runs, r.Id)
		if r.StartedAt.Before(a.first) {
			a.first = r.StartedAt
		}
		if r.StartedAt.After(a.last) {
			a.last = r.StartedAt
		}
	}

	for _, r := range runs {
		st := steps[r.Id]
		// De-duped per run, so one run contributes at most once per subject.
		for _, tool := range failedToolsIn(st) {
			note("tool", tool, r)
		}
		for _, blocked := range governanceBlocksIn(st) {
			note("block", blocked, r)
		}
	}

	out := make([]FailurePattern, 0, len(seen))
	for _, a := range seen {
		if len(a.runs) < minRecurrenceForSkillProposal {
			continue
		}
		out = append(out, FailurePattern{
			Kind:       a.kind,
			Subject:    a.subject,
			Count:      len(a.runs),
			RunIDs:     a.runs,
			FirstAt:    a.first,
			LastAt:     a.last,
			Suggestion: suggestionFor(a.kind, a.subject, len(a.runs)),
		})
	}
	// Most frequent first: that is the order somebody would fix them in.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// suggestionFor names the lever without inventing the wording.
func suggestionFor(kind, subject string, count int) string {
	switch kind {
	case "tool":
		return fmt.Sprintf(
			"%s has failed in %d runs. Either the agent is reaching for it in the wrong situation, "+
				"which is a skill instruction, or it is genuinely broken, which is not.", subject, count)
	case "block":
		return fmt.Sprintf(
			"Governance refused this %d times: %q. If the agent should not be attempting it, say so in a "+
				"skill. If it should be allowed, the autonomy setting is the lever, not the instructions.",
			count, subject)
	default:
		return ""
	}
}

// uniqueStrings preserves order and drops repeats and blanks.
func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// parseSteps decodes a stored run transcript.
//
// A malformed transcript yields nothing rather than an error: this is a review
// screen, and one unreadable run must not hide the other twenty-nine.
func parseSteps(raw string) []stepRecord {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var steps []stepRecord
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil
	}
	return steps
}

// failedToolsIn names the tools that errored, deduped, in call order.
func failedToolsIn(steps []stepRecord) []string {
	var out []string
	for _, s := range steps {
		for _, tc := range s.ToolCalls {
			if strings.TrimSpace(tc.Error) != "" {
				out = append(out, tc.Tool)
			}
		}
	}
	return uniqueStrings(out)
}

// governanceBlocksIn names what governance refused.
//
// Keyed by governance code AND tool, because "blocked" on its own is not
// something anybody can act on, and the pair is: it says which capability the
// agent keeps reaching for that it is not allowed to use.
func governanceBlocksIn(steps []stepRecord) []string {
	var out []string
	for _, s := range steps {
		for _, tc := range s.ToolCalls {
			if isRefusal(tc) {
				out = append(out, fmt.Sprintf("%s on %s", strings.TrimSpace(tc.Governance), tc.Tool))
			}
		}
	}
	return uniqueStrings(out)
}

// isRefusal reports whether governance refused this call outright.
// Approval-required is the system working as configured, not a refusal.
func isRefusal(tc toolCallRecord) bool {
	g := strings.TrimSpace(tc.Governance)
	return g != "" && g != govApprovalRequired
}

// refusalsIn counts every refused call, where governanceBlocksIn names the
// distinct kinds.
func refusalsIn(steps []stepRecord) int {
	n := 0
	for _, s := range steps {
		for _, tc := range s.ToolCalls {
			if isRefusal(tc) {
				n++
			}
		}
	}
	return n
}

// ReviewLearning assembles what the loop has to say about one agent.
func ReviewLearning(ctx context.Context, agentID uuid.UUID, actor Actor) (*LearningReview, error) {
	agent, err := model.GetAgentByID(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if agent == nil {
		return nil, errNotFound
	}
	if !canManage(actor, agent) {
		return nil, errForbidden
	}

	runs, err := model.ListRunsByAgent(ctx, agentID, 100)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-learningWindow)
	review := &LearningReview{AgentID: agentID}
	steps := map[uuid.UUID][]stepRecord{}
	var considered []*model.AgentRun

	for _, r := range runs {
		if r.StartedAt.Before(cutoff) {
			continue
		}
		review.RunsConsidered++
		st := parseSteps(r.Steps)
		steps[r.Id] = st
		considered = append(considered, r)

		if strings.TrimSpace(r.TriggerPrompt) == "" {
			review.RunsWithoutPrompt++
			continue
		}
		if p, ok := proposeScenario(r, failedToolsIn(st)); ok && len(review.Scenarios) < maxProposals {
			review.Scenarios = append(review.Scenarios, p)
		}
	}

	review.Patterns = patternsFrom(considered, steps)
	return review, nil
}
