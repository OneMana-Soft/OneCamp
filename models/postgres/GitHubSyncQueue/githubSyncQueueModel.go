// Package models (GitHubSyncQueue) is the sync queue's row and how one is
// read; its statements are domain/GitHubSyncQueue's, run by models/pgexec.
package models

import (
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

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

// ScanItem reads one item: id, task_id, sync_type, payload, status, attempts,
// next_retry_at, error_message, created_at, updated_at.
func ScanItem(r pgexec.Row) (*GitHubSyncQueueItem, error) {
	var it GitHubSyncQueueItem
	err := r.Scan(&it.ID, &it.TaskID, &it.SyncType, &it.Payload, &it.Status, &it.Attempts, &it.NextRetryAt, &it.ErrorMsg, &it.CreatedAt, &it.UpdatedAt)
	return &it, err
}
