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

	model "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ListByUser returns a user's jobs (optionally filtered by status) for the
// "/remind list" surface and management screens, soonest run_at first.
func ListByUser(ctx context.Context, userUUID uuid.UUID, statuses []string, limit int) ([]*model.ScheduledJob, error) {
	query := `SELECT ` + model.SelectColumns + ` FROM scheduled_jobs WHERE user_uuid = $1 AND deleted_at IS NULL`
	args := []any{userUUID}
	if len(statuses) > 0 {
		query += ` AND status = ANY($2) ORDER BY run_at ASC LIMIT $3`
		args = append(args, pq.Array(statuses), limit)
	} else {
		query += ` ORDER BY run_at ASC LIMIT $2`
		args = append(args, limit)
	}
	return model.ExecJobs(ctx, query, args)
}
