package models

// Agents and their jobs read and stopped many at a time (backend audit U2a:
// stopping an agent's jobs, dismissing a source's runs and announcing the
// change read and wrote each job, and its agent, alone).

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// GetAgentTasksByIDs is the jobs with these ids, in no order.
func GetAgentTasksByIDs(ctx context.Context, ids []uuid.UUID) ([]*AgentTask, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT `+agentTaskColumns+` FROM ai_agent_tasks WHERE id = ANY($1::uuid[])`,
		func(r pgexec.Row) (*AgentTask, error) { return scanAgentTask(r) }, pq.Array(ids))
}

// GetAgentsByIDs is the live agents with these ids, by id.
func GetAgentsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*AiAgent, error) {
	out := make(map[uuid.UUID]*AiAgent, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	agents, err := pgexec.Query(ctx, `SELECT `+agentColumns+` FROM ai_agents WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`,
		func(r pgexec.Row) (*AiAgent, error) { return scanAgent(r) }, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		out[a.Id] = a
	}
	return out, nil
}

// TaskCancel is what RequestAgentTasksCancelByIDs did to one job.
type TaskCancel struct {
	ID    uuid.UUID
	State string
}

// RequestAgentTasksCancelByIDs is RequestAgentTaskCancel for several jobs in
// one statement: each open one stops (queued) or is asked to (running), and
// it answers the jobs it touched with their new state.
func RequestAgentTasksCancelByIDs(ctx context.Context, ids []uuid.UUID, by uuid.UUID) ([]TaskCancel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `UPDATE ai_agent_tasks
		SET cancel_requested_at = COALESCE(cancel_requested_at, now()),
		    cancel_requested_by = COALESCE(cancel_requested_by, $2),
		    state = CASE WHEN state IN ('queued','awaiting_input') THEN 'cancelled' ELSE state END,
		    result = CASE WHEN state IN ('queued','awaiting_input') THEN COALESCE(result, $3) ELSE result END,
		    ended_at = CASE WHEN state IN ('queued','awaiting_input') THEN now() ELSE ended_at END,
		    lease_token = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_token END,
		    lease_expires_at = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_expires_at END,
		    updated_at = now()
		WHERE id = ANY($1::uuid[]) AND state IN ('queued','running','awaiting_input')
		RETURNING id, state`, func(r pgexec.Row) (c TaskCancel, err error) {
		err = r.Scan(&c.ID, &c.State)
		return
	}, pq.Array(ids), by, cancelledBeforeStartNote)
}

// LatestSkillRevisions is each of these skills' newest revision (its name
// and when), in one statement where it was one a skill.
func LatestSkillRevisions(ctx context.Context, skillIDs []uuid.UUID) ([]*SkillRevision, error) {
	if len(skillIDs) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `
		SELECT DISTINCT ON (skill_id) id, skill_id, name, created_at
		FROM ai_agent_skill_revisions
		WHERE skill_id = ANY($1::uuid[])
		ORDER BY skill_id, created_at DESC`, func(r pgexec.Row) (*SkillRevision, error) {
		var rev SkillRevision
		err := r.Scan(&rev.Id, &rev.SkillId, &rev.Name, &rev.CreatedAt)
		return &rev, err
	}, pq.Array(skillIDs))
}
