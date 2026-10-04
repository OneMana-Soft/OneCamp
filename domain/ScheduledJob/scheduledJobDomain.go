package domain

// Scheduled-job reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/ScheduledJob). The user job-list read takes an
// optional status filter — the one dynamic decision — so it belongs here. The
// scan-coupled projection (SelectColumns) stays defined in the model and is
// composed here, keeping a single source of truth.

import (
	"context"
	"fmt"

	model "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ListByUser returns a user's jobs of one type ("" for every type), optionally
// filtered by status, soonest run_at first. The type is filtered here, in SQL:
// filtering after a LIMIT let one kind of job crowd out another (scheduled
// messages hid reminders from "/remind list").
func ListByUser(ctx context.Context, userUUID uuid.UUID, jobType string, statuses []string, limit int) ([]*model.ScheduledJob, error) {
	query := `SELECT ` + model.SelectColumns + ` FROM scheduled_jobs WHERE user_uuid = $1 AND deleted_at IS NULL`
	args := []any{userUUID}
	if jobType != "" {
		args = append(args, jobType)
		query += fmt.Sprintf(` AND job_type = $%d`, len(args))
	}
	if len(statuses) > 0 {
		args = append(args, pq.Array(statuses))
		query += fmt.Sprintf(` AND status = ANY($%d)`, len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY run_at ASC LIMIT $%d`, len(args))
	return model.ExecJobs(ctx, query, args)
}

// CountByUser counts a user's jobs of one type in the given statuses, for caps.
func CountByUser(ctx context.Context, userUUID uuid.UUID, jobType string, statuses []string) (int, error) {
	return model.CountJobs(ctx, `SELECT count(*) FROM scheduled_jobs WHERE user_uuid = $1 AND job_type = $2 AND status = ANY($3) AND deleted_at IS NULL`,
		[]any{userUUID, jobType, pq.Array(statuses)})
}
