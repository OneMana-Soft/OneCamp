package Domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/GitHubSyncQueue"
	"github.com/google/uuid"
)

// AtomicInsertGitHubSyncQueueItem inserts only if no pending/processing item exists for the same task+type.
func AtomicInsertGitHubSyncQueueItem(ctx context.Context, id uuid.UUID, taskID uuid.UUID, syncType string, payload string) (bool, error) {
	inserted, err := models.AtomicInsertGitHubSyncQueueItem(id, taskID, syncType, payload)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AtomicInsertGitHubSyncQueueItem Failed err: %+v", err)
		return false, err
	}
	return inserted, nil
}

// MarkGitHubSyncQueueProcessing marks a queue item as processing.
func MarkGitHubSyncQueueProcessing(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE github_sync_queue SET status = 'processing', attempts = attempts + 1, updated_at = NOW() WHERE id = $1`
	err := models.MarkGitHubSyncQueueProcessing(query, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueueProcessing Failed err: %+v", err)
		return err
	}
	return nil
}

// MarkGitHubSyncQueueCompleted marks a queue item as completed.
func MarkGitHubSyncQueueCompleted(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE github_sync_queue SET status = 'completed', updated_at = NOW() WHERE id = $1`
	err := models.MarkGitHubSyncQueueCompleted(query, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueueCompleted Failed err: %+v", err)
		return err
	}
	return nil
}

// MarkGitHubSyncQueueFailed marks a queue item as failed.
func MarkGitHubSyncQueueFailed(ctx context.Context, id uuid.UUID, errMsg string, nextRetryAt *time.Time) error {
	query := `UPDATE github_sync_queue SET status = 'failed', error_message = $1, next_retry_at = $2, updated_at = NOW() WHERE id = $3`
	err := models.MarkGitHubSyncQueueFailed(query, id, errMsg, nextRetryAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueueFailed Failed err: %+v", err)
		return err
	}
	return nil
}

// MarkGitHubSyncQueuePending marks a queue item as pending with next retry time.
func MarkGitHubSyncQueuePending(ctx context.Context, id uuid.UUID, nextRetryAt time.Time) error {
	query := `UPDATE github_sync_queue SET status = 'pending', next_retry_at = $1, updated_at = NOW() WHERE id = $2`
	err := models.MarkGitHubSyncQueuePending(query, id, nextRetryAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkGitHubSyncQueuePending Failed err: %+v", err)
		return err
	}
	return nil
}

// ReapStaleGitHubSyncQueueItems resets processing items stuck for longer than the timeout.
func ReapStaleGitHubSyncQueueItems(ctx context.Context) (int64, error) {
	query := `UPDATE github_sync_queue SET status = 'pending', updated_at = NOW() WHERE status = 'processing' AND updated_at < NOW() - INTERVAL '10 minutes'`
	n, err := models.ReapStaleGitHubSyncQueueItems(query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ReapStaleGitHubSyncQueueItems Failed err: %+v", err)
		return 0, err
	}
	return n, nil
}

// GetPendingGitHubSyncQueueItems fetches pending sync queue items ordered by creation time.
func GetPendingGitHubSyncQueueItems(ctx context.Context) ([]*models.GitHubSyncQueueItem, error) {
	query := `SELECT id, task_id, sync_type, payload, status, attempts, next_retry_at, error_message, created_at, updated_at FROM github_sync_queue WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= NOW()) ORDER BY created_at ASC LIMIT 10`
	items, err := models.GetPendingGitHubSyncQueueItems(query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetPendingGitHubSyncQueueItems Failed err: %+v", err)
		return nil, err
	}
	return items, nil
}
