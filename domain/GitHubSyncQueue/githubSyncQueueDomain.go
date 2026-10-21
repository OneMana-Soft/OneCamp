package Domain

// The GitHub sync queue's statements, run by models/pgexec: no executor of
// its own (models/postgres/GitHubSyncQueue keeps the row and its scan).

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/GitHubSyncQueue"
	"github.com/google/uuid"
)

// insertUnlessQueued adds an item only when the task has no pending or
// processing item of the same type.
const insertUnlessQueued = `
	INSERT INTO github_sync_queue (id, task_id, sync_type, payload)
	SELECT $1, $2, $3::text, $4
	WHERE NOT EXISTS (
		SELECT 1 FROM github_sync_queue
		WHERE task_id = $2 AND sync_type = $3::text AND status IN ('pending', 'processing')
	)`

// AtomicInsertGitHubSyncQueueItem inserts only if no pending/processing item exists for the same task+type.
func AtomicInsertGitHubSyncQueueItem(ctx context.Context, id uuid.UUID, taskID uuid.UUID, syncType string, payload string) (bool, error) {
	n, err := pgexec.Exec(ctx, insertUnlessQueued, id, taskID, syncType, payload)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AtomicInsertGitHubSyncQueueItem Failed err: %+v", err)
		return false, err
	}
	return n > 0, nil
}

// MarkGitHubSyncQueueCompleted marks a queue item as completed.
func MarkGitHubSyncQueueCompleted(ctx context.Context, id uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE github_sync_queue SET status = 'completed', updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueueCompleted Failed err: %+v", err)
	}
	return err
}

// MarkGitHubSyncQueueFailed marks a queue item as failed.
func MarkGitHubSyncQueueFailed(ctx context.Context, id uuid.UUID, errMsg string, nextRetryAt *time.Time) error {
	_, err := pgexec.Exec(ctx, `UPDATE github_sync_queue SET status = 'failed', error_message = $1, next_retry_at = $2, updated_at = NOW() WHERE id = $3`, errMsg, nextRetryAt, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueueFailed Failed err: %+v", err)
	}
	return err
}

// MarkGitHubSyncQueuePending marks a queue item as pending with next retry time.
func MarkGitHubSyncQueuePending(ctx context.Context, id uuid.UUID, nextRetryAt time.Time) error {
	_, err := pgexec.Exec(ctx, `UPDATE github_sync_queue SET status = 'pending', next_retry_at = $1, updated_at = NOW() WHERE id = $2`, nextRetryAt, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueuePending Failed err: %+v", err)
	}
	return err
}

// ReapStaleGitHubSyncQueueItems resets processing items stuck for longer than the timeout.
func ReapStaleGitHubSyncQueueItems(ctx context.Context) (int64, error) {
	n, err := pgexec.Exec(ctx, `UPDATE github_sync_queue SET status = 'pending', updated_at = NOW() WHERE status = 'processing' AND updated_at < NOW() - INTERVAL '10 minutes'`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ReapStaleGitHubSyncQueueItems Failed err: %+v", err)
		return 0, err
	}
	return n, nil
}

// claimSyncQueueItems takes up to $1 due pending items, oldest first, and
// marks them processing in the same statement. FOR UPDATE SKIP LOCKED leaves
// rows another worker is claiming to that worker, so two API instances never
// take the same item: the old read-then-mark (a SELECT, then an UPDATE ...
// WHERE id = $1 with no status guard) let both process it. attempts is the
// count with this try.
const claimSyncQueueItems = `
	UPDATE github_sync_queue q
	SET status = 'processing', attempts = q.attempts + 1, updated_at = NOW()
	FROM (
		SELECT id FROM github_sync_queue
		WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	) due
	WHERE q.id = due.id
	RETURNING q.id, q.task_id, q.sync_type, q.payload, q.status, q.attempts, q.next_retry_at, q.error_message, q.created_at, q.updated_at`

// ClaimGitHubSyncQueueItems takes up to limit due items for this worker alone,
// already marked processing, oldest first. Each item's Attempts counts this try.
func ClaimGitHubSyncQueueItems(ctx context.Context, limit int) ([]*models.GitHubSyncQueueItem, error) {
	items, err := pgexec.Query(ctx, claimSyncQueueItems, models.ScanItem, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ClaimGitHubSyncQueueItems Failed err: %+v", err)
		return nil, err
	}
	return items, nil
}

// recentFailures splits recent failures into the ones that could never have
// worked ("task has no linked GitHub issue or PR": an enqueue that should
// never have been made) and everything else. A NULL error_message counts as
// "other" rather than falling out of both, since NOT LIKE is NULL for it: the
// two must sum to the failed rows.
const recentFailures = `
	SELECT
		count(*) FILTER (WHERE error_message LIKE '%no linked GitHub issue or PR%'),
		count(*) FILTER (WHERE error_message IS NULL OR error_message NOT LIKE '%no linked GitHub issue or PR%')
	FROM github_sync_queue
	WHERE status = 'failed' AND updated_at >= $1`

// CountRecentFailures is the failures since since: the doomed ones (no linked
// issue or PR) and the others.
func CountRecentFailures(ctx context.Context, since time.Time) (doomed, other int, err error) {
	type counts struct{ doomed, other int }
	c, _, err := pgexec.QueryOne(ctx, recentFailures, func(r pgexec.Row) (counts, error) {
		var c counts
		err := r.Scan(&c.doomed, &c.other)
		return c, err
	}, since)
	return c.doomed, c.other, err
}
