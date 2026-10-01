package models

// Data-access layer for the agent evaluation harness (migration 105): saved
// test scenarios and their scored results. Expectations and per-check detail
// are stored as raw JSON here; the business layer owns their typed shape, so
// this package stays free of any dependency on it.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// EvalScenario is a reusable test case for an agent.
type EvalScenario struct {
	Id           uuid.UUID `json:"id"`
	AgentId      uuid.UUID `json:"agent_id"`
	Name         string    `json:"name"`
	Prompt       string    `json:"prompt"`
	Expectations string    `json:"expectations"` // raw JSON object
	IsActive     bool      `json:"is_active"`
	CreatedBy    uuid.UUID `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// EvalResult is one scored execution of a scenario.
type EvalResult struct {
	Id           uuid.UUID  `json:"id"`
	ScenarioId   uuid.UUID  `json:"scenario_id"`
	AgentId      uuid.UUID  `json:"agent_id"`
	RunId        *uuid.UUID `json:"run_id,omitempty"`
	Passed       bool       `json:"passed"`
	Inconclusive bool       `json:"inconclusive"`
	Score        int        `json:"score"`
	Checks       string     `json:"checks"` // raw JSON array
	Reason       string     `json:"reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// AgentEvalSummary is the latest-suite rollup for an agent's eval badge:
// how many active scenarios, how many of their latest results passed out of
// those that produced a verdict (excluding inconclusive/never-run), and when
// the suite was last evaluated.
type AgentEvalSummary struct {
	ScenarioCount   int        `json:"scenario_count"`
	Passed          int        `json:"passed"`
	Scored          int        `json:"scored"`
	LastEvaluatedAt *time.Time `json:"last_evaluated_at,omitempty"`

	// Stale reports that the agent was edited AFTER these numbers were measured,
	// so the pass rate on screen describes a version of the agent that no longer
	// exists. Derived in the business layer rather than in SQL, because both the
	// single and batch queries would otherwise need the same join and could drift
	// apart; see markStale.
	//
	// This is the field that makes the badge honest. Without it a green 100% sits
	// next to an agent whose instructions were rewritten a minute ago, which is
	// worse than no badge: it is a confident answer to a question nobody asked.
	Stale bool `json:"stale"`
}

// CreateEvalScenario inserts a scenario and returns its id.
func CreateEvalScenario(ctx context.Context, s *EvalScenario) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO ai_agent_eval_scenarios
		(id, agent_id, name, prompt, expectations, is_active, created_by)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, s.AgentId, s.Name, s.Prompt, jsonOrEmpty(s.Expectations), s.IsActive, s.CreatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateEvalScenario err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateEvalScenario updates a scenario's editable fields.
func UpdateEvalScenario(ctx context.Context, s *EvalScenario) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_agent_eval_scenarios
		SET name=$2, prompt=$3, expectations=$4::jsonb, is_active=$5, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		s.Id, s.Name, s.Prompt, jsonOrEmpty(s.Expectations), s.IsActive)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateEvalScenario err: %+v", err)
	}
	return err
}

// GetEvalScenarioByID returns a single non-deleted scenario, or (nil, nil).
func GetEvalScenarioByID(ctx context.Context, id uuid.UUID) (*EvalScenario, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT id, agent_id, name, prompt, expectations::text, is_active, created_by, created_at, updated_at
		FROM ai_agent_eval_scenarios WHERE id=$1 AND deleted_at IS NULL`
	s, err := scanScenario(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetEvalScenarioByID err: %+v", err)
		return nil, err
	}
	return s, nil
}

// ExecEvalScenarios runs a pre-built eval-scenario list query (see
// domain/AIAgent, which owns the optional active-only filter) and scans the
// rows newest-first.
func ExecEvalScenarios(ctx context.Context, query string, args []interface{}) ([]*EvalScenario, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecEvalScenarios err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*EvalScenario
	for rows.Next() {
		s, serr := scanScenario(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CountEvalScenarios returns how many non-deleted scenarios an agent has (used
// to enforce the per-agent cap).
func CountEvalScenarios(ctx context.Context, agentID uuid.UUID) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT COUNT(*) FROM ai_agent_eval_scenarios WHERE agent_id=$1 AND deleted_at IS NULL`, agentID).Scan(&n)
	return n, err
}

// SoftDeleteEvalScenario marks a scenario deleted.
func SoftDeleteEvalScenario(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE ai_agent_eval_scenarios SET deleted_at=NOW(), is_active=false, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteEvalScenario err: %+v", err)
	}
	return err
}

// InsertEvalResult records a scored execution and prunes old results beyond the
// retention bound for that scenario.
func InsertEvalResult(ctx context.Context, r *EvalResult, createdBy *uuid.UUID) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO ai_agent_eval_results
		(id, scenario_id, agent_id, run_id, passed, inconclusive, score, checks, reason, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, r.ScenarioId, r.AgentId, r.RunId, r.Passed, r.Inconclusive, r.Score,
		jsonArrOrEmpty(r.Checks), nullStr(r.Reason), createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InsertEvalResult err: %+v", err)
		return uuid.Nil, err
	}
	pruneEvalResults(dbctx, r.ScenarioId)
	return id, nil
}

// pruneEvalResults keeps only the most recent N results per scenario.
func pruneEvalResults(ctx context.Context, scenarioID uuid.UUID) {
	const keep = 20
	const q = `DELETE FROM ai_agent_eval_results
		WHERE scenario_id=$1 AND id NOT IN (
			SELECT id FROM ai_agent_eval_results WHERE scenario_id=$1
			ORDER BY created_at DESC LIMIT $2)`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, scenarioID, keep); err != nil {
		helpers.LogErrorWithContext(ctx, "models/pruneEvalResults err: %+v", err)
	}
}

// ScenarioVerdict is one scenario's most recent outcome.
type ScenarioVerdict struct {
	ScenarioId   uuid.UUID
	Name         string
	Passed       bool
	Inconclusive bool
	// HasResult separates "never run" from "ran and failed". Folding the two
	// together would report a brand new scenario as a regression the first time
	// it fails, which is not a regression, it is a first measurement.
	HasResult bool
}

// LatestScenarioVerdicts returns the most recent verdict for each active
// scenario, keyed by scenario id.
//
// The rollup above answers "how many passed". This answers "which ones", and the
// difference is the whole point: a suite that fixes one case and breaks another
// reports an unchanged pass rate while something an owner cared about has
// stopped working. Comparing the sets before and after an edit is what turns an
// aggregate into a signal somebody can act on.
//
// Same LATERAL shape as the rollup, so the two cannot disagree about what
// "latest" means.
func LatestScenarioVerdicts(ctx context.Context, agentID uuid.UUID) (map[uuid.UUID]ScenarioVerdict, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT s.id, s.name, r.passed, r.inconclusive, (r.id IS NOT NULL) AS has_result
		FROM ai_agent_eval_scenarios s
		LEFT JOIN LATERAL (
			SELECT id, passed, inconclusive
			FROM ai_agent_eval_results
			WHERE scenario_id = s.id
			ORDER BY created_at DESC LIMIT 1
		) r ON true
		WHERE s.agent_id=$1 AND s.deleted_at IS NULL AND s.is_active = true`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, agentID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/LatestScenarioVerdicts err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]ScenarioVerdict{}
	for rows.Next() {
		var v ScenarioVerdict
		var passed, inconclusive sql.NullBool
		if err := rows.Scan(&v.ScenarioId, &v.Name, &passed, &inconclusive, &v.HasResult); err != nil {
			return nil, err
		}
		v.Passed = passed.Valid && passed.Bool
		v.Inconclusive = inconclusive.Valid && inconclusive.Bool
		out[v.ScenarioId] = v
	}
	return out, rows.Err()
}

// GetAgentEvalSummary computes the latest-suite rollup for an agent's badge in
// one query: for each active scenario it takes the most recent result and
// aggregates passed/scored, where "scored" excludes inconclusive and never-run.
func GetAgentEvalSummary(ctx context.Context, agentID uuid.UUID) (*AgentEvalSummary, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT
			COUNT(*) AS scenario_count,
			COUNT(*) FILTER (WHERE r.passed) AS passed,
			COUNT(*) FILTER (WHERE r.id IS NOT NULL AND NOT r.inconclusive) AS scored,
			MAX(r.created_at) AS last_evaluated
		FROM ai_agent_eval_scenarios s
		LEFT JOIN LATERAL (
			SELECT id, passed, inconclusive, created_at
			FROM ai_agent_eval_results
			WHERE scenario_id = s.id
			ORDER BY created_at DESC LIMIT 1
		) r ON true
		WHERE s.agent_id=$1 AND s.deleted_at IS NULL AND s.is_active = true`
	var sum AgentEvalSummary
	var last sql.NullTime
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, agentID).Scan(
		&sum.ScenarioCount, &sum.Passed, &sum.Scored, &last); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAgentEvalSummary err: %+v", err)
		return nil, err
	}
	if last.Valid {
		sum.LastEvaluatedAt = &last.Time
	}
	return &sum, nil
}

// ExecEvalSummaryBatch runs a pre-built latest-suite rollup query (see
// domain/AIAgent, which owns the optional created_by scoping) and scans it into
// a map keyed by agent id. Only agents with at least one ACTIVE scenario
// appear; the FE treats a missing entry as "no tests".
func ExecEvalSummaryBatch(ctx context.Context, query string, args []interface{}) (map[string]*AgentEvalSummary, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecEvalSummaryBatch err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*AgentEvalSummary)
	for rows.Next() {
		var agentID uuid.UUID
		var sum AgentEvalSummary
		var last sql.NullTime
		if err := rows.Scan(&agentID, &sum.ScenarioCount, &sum.Passed, &sum.Scored, &last); err != nil {
			helpers.LogErrorWithContext(ctx, "models/ExecEvalSummaryBatch scan err: %+v", err)
			return nil, err
		}
		if last.Valid {
			sum.LastEvaluatedAt = &last.Time
		}
		out[agentID.String()] = &sum
	}
	return out, rows.Err()
}

func scanScenario(s scanner) (*EvalScenario, error) {
	var e EvalScenario
	if err := s.Scan(&e.Id, &e.AgentId, &e.Name, &e.Prompt, &e.Expectations, &e.IsActive,
		&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

func jsonOrEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

func jsonArrOrEmpty(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
