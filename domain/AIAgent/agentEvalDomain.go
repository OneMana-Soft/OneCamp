package domain

// Agent evaluation-harness reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/AIAgent). These are the scenario list (optionally
// active-only, for suite runs) and the per-agent latest-suite rollup badge
// (optionally created_by scoped). The optional filters are the dynamic bits, so
// they belong here; the model keeps the scan loops.

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// ListEvalScenariosByAgent returns an agent's scenarios newest first. When
// activeOnly is true, only active scenarios are returned (suite runs).
func ListEvalScenariosByAgent(ctx context.Context, agentID uuid.UUID, activeOnly bool) ([]*model.EvalScenario, error) {
	query := `SELECT id, agent_id, name, prompt, expectations::text, is_active, created_by, created_at, updated_at
		FROM ai_agent_eval_scenarios WHERE agent_id=$1 AND deleted_at IS NULL`
	if activeOnly {
		query += ` AND is_active = true`
	}
	query += ` ORDER BY created_at DESC LIMIT 200`
	return model.ExecEvalScenarios(ctx, query, []interface{}{agentID})
}

// GetAgentEvalSummaryBatch computes the latest-suite rollup for every agent the
// caller may see (createdBy nil = whole workspace) in ONE grouped query, so the
// agents list can show a per-row eval badge without an N+1. Only agents with at
// least one ACTIVE scenario appear; the FE treats a missing entry as "no tests".
func GetAgentEvalSummaryBatch(ctx context.Context, createdBy *uuid.UUID) (map[string]*model.AgentEvalSummary, error) {
	query := `SELECT s.agent_id,
			COUNT(*) AS scenario_count,
			COUNT(*) FILTER (WHERE r.passed) AS passed,
			COUNT(*) FILTER (WHERE r.id IS NOT NULL AND NOT r.inconclusive) AS scored,
			MAX(r.created_at) AS last_evaluated
		FROM ai_agent_eval_scenarios s
		JOIN ai_agents a ON a.id = s.agent_id AND a.deleted_at IS NULL
		LEFT JOIN LATERAL (
			SELECT id, passed, inconclusive, created_at
			FROM ai_agent_eval_results
			WHERE scenario_id = s.id
			ORDER BY created_at DESC LIMIT 1
		) r ON true
		WHERE s.deleted_at IS NULL AND s.is_active = true`
	var args []interface{}
	if createdBy != nil {
		query += ` AND a.created_by = $1`
		args = append(args, *createdBy)
	}
	query += ` GROUP BY s.agent_id`
	return model.ExecEvalSummaryBatch(ctx, query, args)
}
