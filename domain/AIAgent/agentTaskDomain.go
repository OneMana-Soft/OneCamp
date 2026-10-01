package domain

// Agent durable-task reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/AIAgent). These are the cross-agent "active work"
// reads powering the builder's mission-control view (owner/admin scoped) and the
// member-facing "AI teammates working for me" view (actor scoped). Both project
// the same joined agent+task shape, so they share ONE model executor and scan
// loop; only the WHERE/ORDER/args differ, which is exactly what belongs here.

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// activeTaskSelect is the shared projection for the open-durable-job reads. The
// column order is the contract the model's scan loop relies on, so it lives
// with the queries that use it.
const activeTaskSelect = `SELECT t.id, t.agent_id, a.name, a.avatar_key, t.source_type, t.source_id,
		t.surface, t.state, t.attempt, t.last_error, t.next_attempt_at, t.created_at, t.updated_at,
		t.cancel_requested_at IS NOT NULL, a.created_by, t.triggered_by, t.run_as_user_id
		FROM ai_agent_tasks t
		JOIN ai_agents a ON a.id = t.agent_id AND a.deleted_at IS NULL
		WHERE t.state IN ('queued','running','awaiting_input')`

// clampActiveLimit keeps the page size sane (matches the prior model default).
func clampActiveLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 100
	}
	return limit
}

// ListActiveTasks returns the open durable jobs (state in queued / running /
// awaiting_input) across the (non-deleted) agents the caller may see — admins
// pass createdBy=nil for the whole workspace; members pass their own id to see
// only agents they own. Ordered most-recently-updated first (so a job that just
// moved to blocked/working surfaces at the top). Reuses the SAME created_by
// scoping as the runs/stats reads so visibility is consistent across the
// builder.
func ListActiveTasks(ctx context.Context, createdBy *uuid.UUID, limit int) ([]*model.AgentActiveTask, error) {
	limit = clampActiveLimit(limit)

	query := activeTaskSelect
	var args []interface{}
	if createdBy != nil {
		query += ` AND a.created_by = $1 ORDER BY t.updated_at DESC LIMIT $2`
		args = append(args, *createdBy, limit)
	} else {
		query += ` ORDER BY t.updated_at DESC LIMIT $1`
		args = append(args, limit)
	}
	return model.ExecActiveTasks(ctx, query, args)
}

// ListActiveTasksForEntity returns the open durable jobs happening ON one
// surface entity — a channel post, a chat message, or a project task — so the
// place where the work is visible can show (and offer to stop) it, instead of
// making a person leave for a separate panel.
//
// Matching mirrors the continuation engine's: durable mention/thread jobs carry
// the entity in their surface descriptor, task-assignment jobs use source_id, and
// code_pr jobs use a prefixed source_id, which the caller passes in as sourceIDs
// so this layer stays free of that convention. Authorization is the CALLER's job
// (business/AIAgent gates on the surface's own visibility): this is a plain read.
func ListActiveTasksForEntity(ctx context.Context, entityID string, sourceIDs []string, limit int) ([]*model.AgentActiveTask, error) {
	limit = clampActiveLimit(limit)
	query := activeTaskSelect + `
		  AND ((NULLIF($1,'') IS NOT NULL AND (t.surface->>'post_id' = $1 OR t.surface->>'message_id' = $1))
		       OR t.source_id = ANY($2))
		ORDER BY t.updated_at DESC LIMIT $3`
	return model.ExecActiveTasks(ctx, query, []interface{}{entityID, pq.Array(sourceIDs), limit})
}

// ListActiveTasksForActor returns the open durable jobs (queued / running /
// awaiting_input) that a specific PERSON is involved in — the ones they
// triggered (triggered_by) or that run as them (run_as_user_id) — regardless of
// which agent owns them. This powers the member-facing "AI teammates" view:
// what teammates are doing FOR me and where they're waiting ON me, distinct from
// the owner-scoped ListActiveTasks. Deleted agents are excluded; ordered
// most-recently-updated first.
func ListActiveTasksForActor(ctx context.Context, userID uuid.UUID, limit int) ([]*model.AgentActiveTask, error) {
	limit = clampActiveLimit(limit)

	query := activeTaskSelect + `
		  AND (t.triggered_by = $1 OR t.run_as_user_id = $1)
		ORDER BY t.updated_at DESC LIMIT $2`
	return model.ExecActiveTasks(ctx, query, []interface{}{userID, limit})
}
