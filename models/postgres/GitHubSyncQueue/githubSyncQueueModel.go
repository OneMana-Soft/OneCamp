package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AtomicInsertGitHubSyncQueueItem inserts only if no pending/processing item exists for the same task+type.
// Returns true if inserted, false if a duplicate already exists.
func AtomicInsertGitHubSyncQueueItem(id uuid.UUID, taskID uuid.UUID, syncType string, payload string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		INSERT INTO github_sync_queue (id, task_id, sync_type, payload)
		SELECT $1, $2, $3::text, $4
		WHERE NOT EXISTS (
			SELECT 1 FROM github_sync_queue
			WHERE task_id = $2 AND sync_type = $3::text AND status IN ('pending', 'processing')
		)
	`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id, taskID, syncType, payload)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AtomicInsertGitHubSyncQueueItem Failed err: %+v", err)
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// MarkGitHubSyncQueueProcessing marks a queue item as processing.
func MarkGitHubSyncQueueProcessing(query string, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id)
	return err
}

// MarkGitHubSyncQueueCompleted marks a queue item as completed.
func MarkGitHubSyncQueueCompleted(query string, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id)
	return err
}

// MarkGitHubSyncQueueFailed marks a queue item as failed with error message and next retry.
func MarkGitHubSyncQueueFailed(query string, id uuid.UUID, errMsg string, nextRetryAt *time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, errMsg, nextRetryAt, id)
	return err
}

// MarkGitHubSyncQueuePending marks a queue item as pending with next retry time.
func MarkGitHubSyncQueuePending(query string, id uuid.UUID, nextRetryAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, nextRetryAt, id)
	return err
}

// ReapStaleGitHubSyncQueueItems resets processing items that have been stuck for too long.
func ReapStaleGitHubSyncQueueItems(query string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GitHubSyncQueueItem represents a single sync queue row.
type GitHubSyncQueueItem struct {
	ID          uuid.UUID
	TaskID      uuid.UUID
	SyncType    string
	Payload     string
	Status      string
	Attempts    int
	NextRetryAt *time.Time
	ErrorMsg    *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GetPendingGitHubSyncQueueItems fetches pending sync queue items.
func GetPendingGitHubSyncQueueItems(query string) ([]*GitHubSyncQueueItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*GitHubSyncQueueItem
	for rows.Next() {
		var item GitHubSyncQueueItem
		if err := rows.Scan(&item.ID, &item.TaskID, &item.SyncType, &item.Payload, &item.Status, &item.Attempts, &item.NextRetryAt, &item.ErrorMsg, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, &item)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GitHubSyncQueue rows iteration failed err: %+v", err)
		return nil, err
	}
	return items, nil
}

// CountRecentFailures splits recent failures into the ones that could never have
// worked and everything else.
//
// "task has no linked GitHub issue or PR" is the signature of an enqueue that
// should never have been made; anything else is a sync that was legitimately
// attempted and failed, which a workspace can have a few of without being broken.
//
// A NULL error_message counts as "other" rather than falling out of both, since
// NOT LIKE is NULL for it. The two must sum to the number of failed rows, or the
// probe reports a smaller problem than the one there is.
func CountRecentFailures(ctx context.Context, since time.Time) (doomed, other int, err error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT
			count(*) FILTER (WHERE error_message LIKE '%no linked GitHub issue or PR%'),
			count(*) FILTER (WHERE error_message IS NULL OR error_message NOT LIKE '%no linked GitHub issue or PR%')
		FROM github_sync_queue
		WHERE status = 'failed' AND updated_at >= $1`, since).Scan(&doomed, &other)
	return doomed, other, err
}
