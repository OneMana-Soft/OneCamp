package domain

// Agent run-history reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/AIAgent). These are the workspace/owner-scoped run
// rollups powering the builder's activity feed and the agents-list health dots.
// The optional created_by scoping (admins see the whole workspace; members see
// only agents they own) is the single dynamic decision, so it belongs here; the
// model keeps the scan loops.

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// GetAgentHealthBatch returns the compact health signal for every agent the
// caller may see (createdBy nil = whole workspace for admins; otherwise only
// that creator's agents), in a SINGLE grouped query, so the agents list shows a
// per-row status dot without an N+1 of per-agent stats. Runs are joined to
// non-deleted agents so a soft-deleted agent's history is excluded. Only agents
// with at least one run appear; the FE treats a missing entry as "no runs yet".
func GetAgentHealthBatch(ctx context.Context, createdBy *uuid.UUID) (map[string]*model.AgentHealth, error) {
	query := `SELECT r.agent_id,
		COUNT(*) AS total,
		COUNT(*) FILTER (WHERE r.status='succeeded') AS succeeded,
		COUNT(*) FILTER (WHERE r.status='failed') AS failed,
		COUNT(*) FILTER (WHERE r.status='stopped') AS stopped,
		COUNT(*) FILTER (WHERE r.status='running') AS running,
		COUNT(*) FILTER (WHERE r.started_at > NOW() - INTERVAL '7 days') AS last7d_runs,
		MAX(r.started_at) AS last_run_at
		FROM ai_agent_runs r
		JOIN ai_agents a ON a.id = r.agent_id AND a.deleted_at IS NULL`
	var args []interface{}
	if createdBy != nil {
		query += ` WHERE a.created_by = $1`
		args = append(args, *createdBy)
	}
	query += ` GROUP BY r.agent_id`
	return model.ExecAgentHealthBatch(ctx, query, args)
}

// GetWorkspaceAgentStats aggregates agent counts and run health across all
// non-deleted agents, optionally restricted to those created by createdBy
// (members see only their own; admins pass nil for the whole workspace). Builds
// the two aggregate queries here and hands them to the model to execute.
func GetWorkspaceAgentStats(ctx context.Context, createdBy *uuid.UUID) (*model.WorkspaceAgentStats, error) {
	// 1. Agent counts.
	countQ := `SELECT
		COUNT(*) FILTER (WHERE deleted_at IS NULL) AS total,
		COUNT(*) FILTER (WHERE deleted_at IS NULL AND is_active) AS active
		FROM ai_agents`
	var countArgs []interface{}
	if createdBy != nil {
		countQ += ` WHERE created_by = $1`
		countArgs = append(countArgs, *createdBy)
	}

	// 2. Run health across those agents.
	runQ := `SELECT
		COUNT(*) AS total,
		COUNT(*) FILTER (WHERE r.status='succeeded') AS succeeded,
		COUNT(*) FILTER (WHERE r.status='failed') AS failed,
		COUNT(*) FILTER (WHERE r.status='stopped') AS stopped,
		COUNT(*) FILTER (WHERE r.status='running') AS running,
		COALESCE(SUM(r.tokens),0) AS total_tokens,
		COUNT(*) FILTER (WHERE r.started_at > NOW() - INTERVAL '7 days') AS last7d_runs,
		COALESCE(SUM(r.tokens) FILTER (WHERE r.started_at > NOW() - INTERVAL '7 days'),0) AS last7d_tokens
		FROM ai_agent_runs r
		JOIN ai_agents a ON a.id = r.agent_id AND a.deleted_at IS NULL`
	var runArgs []interface{}
	if createdBy != nil {
		runQ += ` AND a.created_by = $1`
		runArgs = append(runArgs, *createdBy)
	}
	return model.ExecWorkspaceAgentStats(ctx, countQ, countArgs, runQ, runArgs)
}

// ListRecentRuns returns the most recent agent runs across all (non-deleted)
// agents the caller may see — admins pass createdBy=nil for the whole
// workspace; members pass their own id to see only the agents they own. Ordered
// newest-first. Reuses the same created_by scoping as GetAgentHealthBatch, so
// visibility is consistent across the builder.
func ListRecentRuns(ctx context.Context, createdBy *uuid.UUID, limit int) ([]*model.AgentRunActivity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `SELECT r.id, r.agent_id, r.trigger_source, r.run_as_user_id, r.status, r.steps,
		r.step_count, r.tokens, r.result, r.error, r.started_at, r.ended_at,
		a.name, a.avatar_key
		FROM ai_agent_runs r
		JOIN ai_agents a ON a.id = r.agent_id AND a.deleted_at IS NULL`
	var args []interface{}
	if createdBy != nil {
		query += ` WHERE a.created_by = $1 ORDER BY r.started_at DESC LIMIT $2`
		args = append(args, *createdBy, limit)
	} else {
		query += ` ORDER BY r.started_at DESC LIMIT $1`
		args = append(args, limit)
	}
	return model.ExecRecentRuns(ctx, query, args)
}
