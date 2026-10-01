package business

// Agent evaluation harness — scoring core.
//
// Turns the one-shot "test" panel into saved, scored scenarios: a scenario is a
// prompt plus declarative expectations, and an eval scores one RunOutcome
// against those expectations deterministically. This file holds the PURE
// scoring logic (no I/O), so it is unit-tested DB-free; persistence (scenarios
// + results) and execution (run-as-owner dry-run) layer on top.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// Limits keep eval scenarios bounded.
const (
	maxScenariosPerAgent = 50
	maxScenarioNameLen   = 120
	maxScenarioPromptLen = 4000
	maxPhrasesPerCheck   = 20
	maxToolsPerCheck     = 20
	maxPhraseLen         = 200
)

// Expectations is the declarative assertion set saved on a scenario. Every field
// is optional; an empty Expectations is a smoke test that only requires a
// non-error terminal status.
type Expectations struct {
	// MustContain: the answer must include each phrase (case-insensitive).
	MustContain []string `json:"must_contain,omitempty"`
	// MustNotContain: the answer must include none of these phrases.
	MustNotContain []string `json:"must_not_contain,omitempty"`
	// ExpectedTools: each named tool must appear in the run's tool calls.
	ExpectedTools []string `json:"expected_tools,omitempty"`
	// ForbiddenTools: none of these tools may be called.
	ForbiddenTools []string `json:"forbidden_tools,omitempty"`
	// ExpectedStatus: the run's terminal status must equal this (e.g.
	// "succeeded"). Empty means "any non-failed, non-stopped terminal status".
	ExpectedStatus string `json:"expected_status,omitempty"`
}

// ExpectationResult is the per-assertion outcome with a human-readable reason.
type ExpectationResult struct {
	Kind   string `json:"kind"`   // e.g. "must_contain", "expected_tools"
	Target string `json:"target"` // the specific phrase/tool/status checked
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}

// ScoreResult is the overall eval outcome for one run against one scenario.
type ScoreResult struct {
	Passed       bool                `json:"passed"`
	Score        int                 `json:"score"` // 0..100, fraction of checks met
	Inconclusive bool                `json:"inconclusive,omitempty"`
	Reason       string              `json:"reason,omitempty"` // set when inconclusive
	Checks       []ExpectationResult `json:"checks"`
}

// inconclusiveReason maps a run that could not produce a verdict (throttle,
// open circuit, token budget) to a reason, so the harness records "inconclusive"
// rather than a false fail. Returns "" when the run is conclusive.
func inconclusiveReason(outcome *RunOutcome) string {
	if outcome == nil {
		return "no run outcome"
	}
	r := strings.ToLower(outcome.Error)
	switch {
	case strings.Contains(r, "circuit"):
		return "AI temporarily unavailable (circuit open)"
	case strings.Contains(r, "rate limit"):
		return "rate limited"
	case strings.Contains(r, "token budget"):
		return "AI token budget reached"
	default:
		return ""
	}
}

// ScoreRun scores a run outcome against a scenario's expectations. Pure: no I/O.
// An inconclusive run (throttle/breaker/budget) returns Inconclusive=true with a
// reason and is neither pass nor fail. With no expectations, the run passes iff
// it reached a non-error terminal status (a smoke test).
func ScoreRun(outcome *RunOutcome, exp Expectations) ScoreResult {
	if reason := inconclusiveReason(outcome); reason != "" {
		return ScoreResult{Inconclusive: true, Reason: reason}
	}

	answer := strings.ToLower(strings.TrimSpace(outcome.Result))
	toolSet := make(map[string]bool, len(outcome.ToolsUsed))
	for _, t := range outcome.ToolsUsed {
		toolSet[t] = true
	}

	var checks []ExpectationResult

	for _, phrase := range exp.MustContain {
		p := strings.TrimSpace(phrase)
		if p == "" {
			continue
		}
		ok := strings.Contains(answer, strings.ToLower(p))
		checks = append(checks, ExpectationResult{
			Kind: "must_contain", Target: p, Passed: ok,
			Reason: passReason(ok, "answer includes the phrase", "answer is missing the phrase"),
		})
	}

	for _, phrase := range exp.MustNotContain {
		p := strings.TrimSpace(phrase)
		if p == "" {
			continue
		}
		ok := !strings.Contains(answer, strings.ToLower(p))
		checks = append(checks, ExpectationResult{
			Kind: "must_not_contain", Target: p, Passed: ok,
			Reason: passReason(ok, "answer correctly omits the phrase", "answer contains a forbidden phrase"),
		})
	}

	for _, tool := range exp.ExpectedTools {
		t := strings.TrimSpace(tool)
		if t == "" {
			continue
		}
		ok := toolSet[t]
		checks = append(checks, ExpectationResult{
			Kind: "expected_tools", Target: t, Passed: ok,
			Reason: passReason(ok, "tool was used", "tool was not used"),
		})
	}

	for _, tool := range exp.ForbiddenTools {
		t := strings.TrimSpace(tool)
		if t == "" {
			continue
		}
		ok := !toolSet[t]
		checks = append(checks, ExpectationResult{
			Kind: "forbidden_tools", Target: t, Passed: ok,
			Reason: passReason(ok, "forbidden tool was not used", "forbidden tool was used"),
		})
	}

	if status := strings.TrimSpace(exp.ExpectedStatus); status != "" {
		ok := outcome.Status == status
		checks = append(checks, ExpectationResult{
			Kind: "expected_status", Target: status, Passed: ok,
			Reason: passReason(ok, "run reached the expected status", "run ended in status "+outcome.Status),
		})
	}

	// No explicit expectations: a smoke test that only requires a clean finish.
	if len(checks) == 0 {
		ok := outcome.Status == model.RunSucceeded
		return ScoreResult{
			Passed: ok,
			Score:  boolScore(ok),
			Checks: []ExpectationResult{{
				Kind: "smoke", Target: model.RunSucceeded, Passed: ok,
				Reason: passReason(ok, "run succeeded", "run ended in status "+outcome.Status),
			}},
		}
	}

	passed := 0
	for _, c := range checks {
		if c.Passed {
			passed++
		}
	}
	return ScoreResult{
		Passed: passed == len(checks),
		Score:  passed * 100 / len(checks),
		Checks: checks,
	}
}

func passReason(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func boolScore(ok bool) int {
	if ok {
		return 100
	}
	return 0
}

// ─── Scenario management + execution ────────────────────────────────────────

// ScenarioInput is the create/update payload for an eval scenario.
type ScenarioInput struct {
	Name         string       `json:"name"`
	Prompt       string       `json:"prompt"`
	Expectations Expectations `json:"expectations"`
	IsActive     bool         `json:"is_active"`
}

// ScenarioView is a scenario returned to the API, with expectations decoded.
type ScenarioView struct {
	Id           uuid.UUID    `json:"id"`
	AgentId      uuid.UUID    `json:"agent_id"`
	Name         string       `json:"name"`
	Prompt       string       `json:"prompt"`
	Expectations Expectations `json:"expectations"`
	IsActive     bool         `json:"is_active"`
	CreatedAt    string       `json:"created_at"`
	UpdatedAt    string       `json:"updated_at"`
}

// ScenarioRunResult is one scored scenario execution returned to the caller.
type ScenarioRunResult struct {
	ScenarioId uuid.UUID   `json:"scenario_id"`
	Name       string      `json:"name"`
	RunId      uuid.UUID   `json:"run_id"`
	Result     ScoreResult `json:"result"`
}

// SuiteRunResult aggregates a full-suite eval.
type SuiteRunResult struct {
	AgentId   uuid.UUID           `json:"agent_id"`
	Total     int                 `json:"total"`
	Passed    int                 `json:"passed"`
	Scored    int                 `json:"scored"` // conclusive results
	Scenarios []ScenarioRunResult `json:"scenarios"`
}

// validateExpectations enforces sane bounds and trims blanks. Returns the
// cleaned set.
func validateExpectations(e Expectations) (Expectations, error) {
	clean := func(label string, in []string) ([]string, error) {
		out := make([]string, 0, len(in))
		for _, v := range in {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if len(v) > maxPhraseLen {
				return nil, fmt.Errorf("%s entry is too long", label)
			}
			out = append(out, v)
		}
		return out, nil
	}
	var err error
	if e.MustContain, err = clean("must_contain", e.MustContain); err != nil {
		return e, err
	}
	if e.MustNotContain, err = clean("must_not_contain", e.MustNotContain); err != nil {
		return e, err
	}
	if e.ExpectedTools, err = clean("expected_tools", e.ExpectedTools); err != nil {
		return e, err
	}
	if e.ForbiddenTools, err = clean("forbidden_tools", e.ForbiddenTools); err != nil {
		return e, err
	}
	if len(e.MustContain) > maxPhrasesPerCheck || len(e.MustNotContain) > maxPhrasesPerCheck {
		return e, fmt.Errorf("too many phrases in an expectation")
	}
	if len(e.ExpectedTools) > maxToolsPerCheck || len(e.ForbiddenTools) > maxToolsPerCheck {
		return e, fmt.Errorf("too many tools in an expectation")
	}
	e.ExpectedStatus = strings.TrimSpace(e.ExpectedStatus)
	if e.ExpectedStatus != "" && !validTerminalStatus(e.ExpectedStatus) {
		return e, fmt.Errorf("invalid expected_status")
	}
	return e, nil
}

func validTerminalStatus(s string) bool {
	switch s {
	case model.RunSucceeded, model.RunFailed, model.RunStopped:
		return true
	default:
		return false
	}
}

func validateScenarioInput(in *ScenarioInput) (string, Expectations, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", Expectations{}, fmt.Errorf("name is required")
	}
	if len(name) > maxScenarioNameLen {
		return "", Expectations{}, fmt.Errorf("name is too long")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return "", Expectations{}, fmt.Errorf("prompt is required")
	}
	if len(prompt) > maxScenarioPromptLen {
		return "", Expectations{}, fmt.Errorf("prompt is too long")
	}
	exp, err := validateExpectations(in.Expectations)
	if err != nil {
		return "", Expectations{}, err
	}
	return name, exp, nil
}

func toScenarioView(s *model.EvalScenario) ScenarioView {
	var exp Expectations
	_ = json.Unmarshal([]byte(s.Expectations), &exp)
	return ScenarioView{
		Id: s.Id, AgentId: s.AgentId, Name: s.Name, Prompt: s.Prompt,
		Expectations: exp, IsActive: s.IsActive,
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: s.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// CreateScenario adds a scenario to an agent the actor may manage.
func CreateScenario(ctx context.Context, agentID uuid.UUID, in ScenarioInput, actor Actor) (*ScenarioView, error) {
	if _, err := GetAgent(ctx, agentID, actor); err != nil {
		return nil, err
	}
	name, exp, err := validateScenarioInput(&in)
	if err != nil {
		return nil, err
	}
	if n, cerr := model.CountEvalScenarios(ctx, agentID); cerr == nil && n >= maxScenariosPerAgent {
		return nil, fmt.Errorf("this agent already has the maximum number of scenarios")
	}
	expJSON, _ := json.Marshal(exp)
	s := &model.EvalScenario{
		AgentId: agentID, Name: name, Prompt: strings.TrimSpace(in.Prompt),
		Expectations: string(expJSON), IsActive: in.IsActive, CreatedBy: actor.UserID,
	}
	id, err := model.CreateEvalScenario(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("failed to create scenario")
	}
	created, err := model.GetEvalScenarioByID(ctx, id)
	if err != nil || created == nil {
		return nil, fmt.Errorf("failed to load scenario")
	}
	v := toScenarioView(created)
	return &v, nil
}

// scenarioWithGate loads a scenario and verifies the actor may manage its agent.
func scenarioWithGate(ctx context.Context, scenarioID uuid.UUID, actor Actor) (*model.EvalScenario, error) {
	s, err := model.GetEvalScenarioByID(ctx, scenarioID)
	if err != nil {
		return nil, fmt.Errorf("failed to load scenario")
	}
	if s == nil {
		return nil, errNotFound
	}
	if _, gerr := GetAgent(ctx, s.AgentId, actor); gerr != nil {
		return nil, gerr
	}
	return s, nil
}

// UpdateScenario edits a scenario the actor may manage.
func UpdateScenario(ctx context.Context, scenarioID uuid.UUID, in ScenarioInput, actor Actor) (*ScenarioView, error) {
	s, err := scenarioWithGate(ctx, scenarioID, actor)
	if err != nil {
		return nil, err
	}
	name, exp, verr := validateScenarioInput(&in)
	if verr != nil {
		return nil, verr
	}
	expJSON, _ := json.Marshal(exp)
	s.Name = name
	s.Prompt = strings.TrimSpace(in.Prompt)
	s.Expectations = string(expJSON)
	s.IsActive = in.IsActive
	if uerr := model.UpdateEvalScenario(ctx, s); uerr != nil {
		return nil, fmt.Errorf("failed to update scenario")
	}
	updated, _ := model.GetEvalScenarioByID(ctx, scenarioID)
	if updated == nil {
		return nil, fmt.Errorf("failed to load scenario")
	}
	v := toScenarioView(updated)
	return &v, nil
}

// DeleteScenario soft-deletes a scenario the actor may manage.
func DeleteScenario(ctx context.Context, scenarioID uuid.UUID, actor Actor) error {
	if _, err := scenarioWithGate(ctx, scenarioID, actor); err != nil {
		return err
	}
	return model.SoftDeleteEvalScenario(ctx, scenarioID)
}

// ListScenarios returns an agent's scenarios for an actor who may manage it.
func ListScenarios(ctx context.Context, agentID uuid.UUID, actor Actor) ([]ScenarioView, error) {
	if _, err := GetAgent(ctx, agentID, actor); err != nil {
		return nil, err
	}
	rows, err := agentDomain.ListEvalScenariosByAgent(ctx, agentID, false)
	if err != nil {
		return nil, fmt.Errorf("failed to load scenarios")
	}
	out := make([]ScenarioView, 0, len(rows))
	for _, s := range rows {
		out = append(out, toScenarioView(s))
	}
	return out, nil
}

// EvalSummary returns the latest-suite rollup for an agent's eval badge.
func EvalSummary(ctx context.Context, agentID uuid.UUID, actor Actor) (*model.AgentEvalSummary, error) {
	agent, err := GetAgent(ctx, agentID, actor)
	if err != nil {
		return nil, err
	}
	sum, err := model.GetAgentEvalSummary(ctx, agentID)
	if err != nil {
		return nil, err
	}
	markStale(sum, agent.UpdatedAt)
	return sum, nil
}

// markStale flags a summary whose numbers predate the agent's last edit.
//
// One function rather than a term in two SQL queries, so the single-agent and
// batch paths cannot disagree about what stale means. A summary with nothing
// scored yet is not stale, it is simply unmeasured, and saying "out of date"
// about a measurement that never happened would be wrong.
func markStale(sum *model.AgentEvalSummary, agentUpdatedAt time.Time) {
	if sum == nil || sum.LastEvaluatedAt == nil {
		return
	}
	sum.Stale = agentUpdatedAt.After(*sum.LastEvaluatedAt)
}

// EvalSummaryBatch returns the latest-suite rollup for every agent the actor may
// see, keyed by agent id, in one query. Powers the per-row eval badge in the
// agents list. Same actor scoping as the rest of the builder.
func EvalSummaryBatch(ctx context.Context, actor Actor) (map[string]*model.AgentEvalSummary, error) {
	var createdBy *uuid.UUID
	if !actor.IsAdmin {
		id := actor.UserID
		createdBy = &id
	}
	summaries, err := agentDomain.GetAgentEvalSummaryBatch(ctx, createdBy)
	if err != nil {
		return nil, err
	}

	// Staleness needs each agent's updated_at, which the rollup query does not
	// carry. Loading the agents the actor can already see is one more read for
	// the whole list, and it keeps the definition of stale in markStale rather
	// than duplicating a join into the batch SQL.
	agents, err := ListAgents(ctx, actor)
	if err != nil {
		// The rollup is still useful without the flag; a missing "out of date"
		// mark is a smaller failure than an empty badge column.
		helpers.LogErrorWithContext(ctx, "business/EvalSummaryBatch: could not load agents for staleness: %v", err)
		return summaries, nil
	}
	for _, a := range agents {
		if sum, ok := summaries[a.Id.String()]; ok {
			markStale(sum, a.UpdatedAt)
		}
	}
	return summaries, nil
}

// runAndScore executes one scenario in DRY-RUN (writes never happen), scores the
// outcome, and persists the result. Shared by single-scenario and suite runs.
func runAndScore(ctx context.Context, agent *model.AiAgent, s *model.EvalScenario, actor Actor) ScenarioRunResult {
	var exp Expectations
	_ = json.Unmarshal([]byte(s.Expectations), &exp)

	// Dry-run: read-only tools execute for a realistic transcript; writes never
	// happen. Runs AS the agent's owner with per-call permission re-checks.
	outcome := RunAgent(ctx, agent, "eval", s.Prompt, true)
	score := ScoreRun(outcome, exp)

	checksJSON, _ := json.Marshal(score.Checks)
	res := &model.EvalResult{
		ScenarioId: s.Id, AgentId: agent.Id, Passed: score.Passed,
		Inconclusive: score.Inconclusive, Score: score.Score,
		Checks: string(checksJSON), Reason: score.Reason,
	}
	if outcome != nil && outcome.RunID != uuid.Nil {
		rid := outcome.RunID
		res.RunId = &rid
	}
	actorID := actor.UserID
	if _, err := model.InsertEvalResult(ctx, res, &actorID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/runAndScore persist failed: %v", err)
	}
	rid := uuid.Nil
	if outcome != nil {
		rid = outcome.RunID
	}
	return ScenarioRunResult{ScenarioId: s.Id, Name: s.Name, RunId: rid, Result: score}
}

// RunScenario executes and scores one scenario the actor may manage.
func RunScenario(ctx context.Context, scenarioID uuid.UUID, actor Actor) (*ScenarioRunResult, error) {
	s, err := scenarioWithGate(ctx, scenarioID, actor)
	if err != nil {
		return nil, err
	}
	agent, err := model.GetAgentByID(ctx, s.AgentId)
	if err != nil || agent == nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	r := runAndScore(ctx, agent, s, actor)
	return &r, nil
}

// RunSuite executes every active scenario for an agent the actor may manage and
// returns the aggregate pass-rate plus each scenario's result.
func RunSuite(ctx context.Context, agentID uuid.UUID, actor Actor) (*SuiteRunResult, error) {
	agent, err := GetAgent(ctx, agentID, actor)
	if err != nil {
		return nil, err
	}
	scenarios, err := agentDomain.ListEvalScenariosByAgent(ctx, agentID, true)
	if err != nil {
		return nil, fmt.Errorf("failed to load scenarios")
	}
	out := &SuiteRunResult{AgentId: agentID, Total: len(scenarios)}
	for _, s := range scenarios {
		r := runAndScore(ctx, agent, s, actor)
		out.Scenarios = append(out.Scenarios, r)
		if r.Result.Inconclusive {
			continue
		}
		out.Scored++
		if r.Result.Passed {
			out.Passed++
		}
	}
	return out, nil
}
