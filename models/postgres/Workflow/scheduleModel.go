package models

// A schedule rule's waiting job. Rules that run on time use the one queue for
// delayed work (scheduled_jobs, business/Scheduler) rather than a claim loop
// of their own. Each live schedule rule has one waiting job, which fires and
// then names the next, as the calendar's reminders do (migration 350's
// partial unique index makes planning again a move).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ScheduleJobType is a schedule rule's job in scheduled_jobs.
const ScheduleJobType = "workflow_schedule"

// SchedulePayload is what a schedule rule's job carries.
type SchedulePayload struct {
	WorkflowID string `json:"workflow_id"`
}

// PlanScheduleJob plans a rule's next run at runAt, as owner: a new waiting
// job, or the rule's waiting job moved there.
func PlanScheduleJob(ctx context.Context, workflowID, owner uuid.UUID, runAt time.Time) error {
	return PlanScheduleJobs(ctx, []SchedulePlan{{WorkflowID: workflowID, Owner: owner, RunAt: runAt}})
}

// SchedulePlan is one rule's next run, to plan.
type SchedulePlan struct {
	WorkflowID, Owner uuid.UUID
	RunAt             time.Time
}

// PlanScheduleJobs plans several rules' next runs in one statement (the
// hourly pass plans every rule left without a job at once).
func PlanScheduleJobs(ctx context.Context, plans []SchedulePlan) error {
	if len(plans) == 0 {
		return nil
	}
	ids, owners, payloads, times := make([]string, len(plans)), make([]string, len(plans)), make([]string, len(plans)), make([]string, len(plans))
	for i, p := range plans {
		payload, err := json.Marshal(SchedulePayload{WorkflowID: p.WorkflowID.String()})
		if err != nil {
			return err
		}
		ids[i], owners[i], payloads[i], times[i] = uuid.NewString(), p.Owner.String(), string(payload), p.RunAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO scheduled_jobs (id, job_type, user_uuid, payload, run_at, max_attempts)
		SELECT unnest($1::uuid[]), 'workflow_schedule', unnest($2::uuid[]), unnest($3::jsonb[]), unnest($4::timestamptz[]), 3
		ON CONFLICT ((payload->>'workflow_id'))
		  WHERE job_type = 'workflow_schedule' AND status = 'pending' AND deleted_at IS NULL
		DO UPDATE SET user_uuid = EXCLUDED.user_uuid, run_at = EXCLUDED.run_at, attempts = 0, last_error = NULL, updated_at = NOW()`,
		pq.Array(ids), pq.Array(owners), pq.Array(payloads), pq.Array(times))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Workflow.PlanScheduleJobs err: %+v", err)
	}
	return err
}

// CancelScheduleJob cancels a rule's waiting job, if it has one.
func CancelScheduleJob(ctx context.Context, workflowID uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE scheduled_jobs SET status = 'cancelled', updated_at = NOW()
		WHERE job_type = 'workflow_schedule' AND status = 'pending' AND deleted_at IS NULL
		  AND payload->>'workflow_id' = $1`, workflowID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Workflow.CancelScheduleJob err: %+v", err)
	}
	return err
}

// UnplannedSchedules is live schedule rules that are turned on and have no
// job waiting or running, oldest first, at most limit: what the hourly pass
// plans again.
func UnplannedSchedules(ctx context.Context, limit int) ([]*Workflow, error) {
	out, err := pgexec.Query(ctx, `SELECT `+selectColumns+` FROM workflows w
		WHERE w.deleted_at IS NULL AND w.is_active AND w.trigger_type = 'schedule'
		  AND NOT EXISTS (SELECT 1 FROM scheduled_jobs j
		      WHERE j.job_type = 'workflow_schedule' AND j.status IN ('pending', 'running') AND j.deleted_at IS NULL
		        AND j.payload->>'workflow_id' = w.id::text)
		ORDER BY w.created_at, w.id LIMIT $1`, func(r pgexec.Row) (*Workflow, error) { return scanWorkflow(r) }, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Workflow.UnplannedSchedules err: %+v", err)
		return nil, err
	}
	return out, nil
}
