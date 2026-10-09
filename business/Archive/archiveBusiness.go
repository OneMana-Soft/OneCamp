package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ArchiveAlreadyRunningError is returned when an archive job is already in progress.
type ArchiveAlreadyRunningError struct {
	EntityType string
}

func (e *ArchiveAlreadyRunningError) Error() string {
	return fmt.Sprintf("archive job already running for entity type: %s", e.EntityType)
}

// ErrPolicyNotFound is returned when an archive policy does not exist.
var ErrPolicyNotFound = errors.New("policy not found")

// validEntityTypes is the closed allowlist of entity types that can be archived.
// Keep this in sync with the FE constants.
var validEntityTypes = map[string]bool{
	"posts": true, "chats": true, "tasks": true,
	"recordings": true, "attachments": true, "docs": true,
}

// entityLabels provides human-readable names for entity types.
var entityLabels = map[string]string{
	"posts": "Post", "chats": "Chat", "tasks": "Task",
	"docs": "Document", "recordings": "Recording", "attachments": "Attachment",
}

// entitySupportsUndo indicates which entity types support job undo.
var entitySupportsUndo = map[string]bool{
	"posts": true, "chats": true, "tasks": true, "attachments": true,
}

// entityRestoreQuery holds the RETURNING id::text query template per entity type.
// The %s placeholder is for the IN-list placeholders (e.g. "$1,$2,$3").
var entityRestoreQuery = map[string]string{
	"posts":       `UPDATE posts SET deleted_at = NULL, updated_at = NOW() WHERE id::text IN (%s) AND deleted_at IS NOT NULL RETURNING id::text`,
	"chats":       `UPDATE chats SET deleted_at = NULL, updated_at = NOW() WHERE id::text IN (%s) AND deleted_at IS NOT NULL RETURNING id::text`,
	"tasks":       `UPDATE tasks SET deleted_at = NULL, updated_at = NOW() WHERE id::text IN (%s) AND deleted_at IS NOT NULL RETURNING id::text`,
	"attachments": `UPDATE attachments SET deleted_at = NULL WHERE id::text IN (%s) AND deleted_at IS NOT NULL RETURNING id::text`, // no updated_at column
}

// entityRecentCountQuery holds the COUNT query per entity type for recently-archived listings.
var entityRecentCountQuery = map[string]string{
	"posts":       `SELECT COUNT(*) FROM posts WHERE deleted_at IS NOT NULL AND deleted_at > $1`,
	"chats":       `SELECT COUNT(*) FROM chats WHERE deleted_at IS NOT NULL AND deleted_at > $1`,
	"tasks":       `SELECT COUNT(*) FROM tasks WHERE deleted_at IS NOT NULL AND deleted_at > $1`,
	"attachments": `SELECT COUNT(*) FROM attachments WHERE deleted_at IS NOT NULL AND deleted_at > $1`,
}

// entityRecentListQuery holds the list query per entity type for recently-archived listings.
var entityRecentListQuery = map[string]string{
	"posts":       `SELECT id::text, deleted_at FROM posts WHERE deleted_at IS NOT NULL AND deleted_at > $1 ORDER BY deleted_at DESC LIMIT $2 OFFSET $3`,
	"chats":       `SELECT id::text, deleted_at FROM chats WHERE deleted_at IS NOT NULL AND deleted_at > $1 ORDER BY deleted_at DESC LIMIT $2 OFFSET $3`,
	"tasks":       `SELECT id::text, deleted_at FROM tasks WHERE deleted_at IS NOT NULL AND deleted_at > $1 ORDER BY deleted_at DESC LIMIT $2 OFFSET $3`,
	"attachments": `SELECT id::text, deleted_at FROM attachments WHERE deleted_at IS NOT NULL AND deleted_at > $1 ORDER BY deleted_at DESC LIMIT $2 OFFSET $3`,
}

// ArchivePolicy represents an archive policy.
type ArchivePolicy struct {
	Id                          uuid.UUID `json:"id"`
	EntityType                  string    `json:"entity_type"`
	RetentionDays               int       `json:"retention_days"`
	AutoArchive                 bool      `json:"auto_archive"`
	ArchiveCompletedTasks       bool      `json:"archive_completed_tasks"`
	ArchiveInactiveChannelsDays int       `json:"archive_inactive_channels_days"`
	CompressAttachments         bool      `json:"compress_attachments"`
	// Permanent removal, see purge.go. 0 keeps archived items forever.
	PurgeAfterDays int        `json:"purge_after_days"`
	PurgedCount    int64      `json:"purged_count"`
	PurgedBytes    int64      `json:"purged_bytes"`
	LastPurgedAt   *time.Time `json:"last_purged_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ArchiveJob represents an archive job execution.
type ArchiveJob struct {
	Id             uuid.UUID  `json:"id"`
	EntityType     string     `json:"entity_type"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	ItemsProcessed int        `json:"items_processed"`
	ItemsArchived  int        `json:"items_archived"`
	ItemsFailed    int        `json:"items_failed"`
	ErrorMessage   *string    `json:"error_message,omitempty"`
	TriggeredBy    *uuid.UUID `json:"triggered_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ArchiveStats represents archive statistics.
// TODO: add Dgraph-side counts for docs and recordings.
type ArchiveStats struct {
	TotalArchivedPosts       int64 `json:"total_archived_posts"`
	TotalArchivedChats       int64 `json:"total_archived_chats"`
	TotalArchivedTasks       int64 `json:"total_archived_tasks"`
	TotalArchivedRecordings  int64 `json:"total_archived_recordings"`
	TotalArchivedAttachments int64 `json:"total_archived_attachments"`
	TotalArchivedDocs        int64 `json:"total_archived_docs"`
}

// PolicyUpdateInput for updating a policy.
type PolicyUpdateInput struct {
	RetentionDays               *int  `json:"retention_days,omitempty"`
	AutoArchive                 *bool `json:"auto_archive,omitempty"`
	ArchiveCompletedTasks       *bool `json:"archive_completed_tasks,omitempty"`
	ArchiveInactiveChannelsDays *int  `json:"archive_inactive_channels_days,omitempty"`
	CompressAttachments         *bool `json:"compress_attachments,omitempty"`
	PurgeAfterDays              *int  `json:"purge_after_days,omitempty"`
}

// GetArchivePolicies returns all archive policies.
func GetArchivePolicies(ctx context.Context) ([]*ArchivePolicy, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, entity_type, retention_days, auto_archive, archive_completed_tasks, archive_inactive_channels_days, compress_attachments, purge_after_days, purged_count, purged_bytes, last_purged_at, created_at, updated_at
		FROM archive_policies
		ORDER BY entity_type
	`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetArchivePolicies Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var policies []*ArchivePolicy
	for rows.Next() {
		var p ArchivePolicy
		if err := rows.Scan(&p.Id, &p.EntityType, &p.RetentionDays, &p.AutoArchive, &p.ArchiveCompletedTasks, &p.ArchiveInactiveChannelsDays, &p.CompressAttachments, &p.PurgeAfterDays, &p.PurgedCount, &p.PurgedBytes, &p.LastPurgedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		policies = append(policies, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return policies, nil
}

// UpdateArchivePolicy updates a specific policy.
func UpdateArchivePolicy(ctx context.Context, entityType string, input PolicyUpdateInput) error {
	if input.RetentionDays != nil {
		if *input.RetentionDays < 7 {
			return fmt.Errorf("retention_days must be at least 7")
		}
		if *input.RetentionDays > 3650 {
			return fmt.Errorf("retention_days must be at most 3650")
		}
	}
	if input.ArchiveInactiveChannelsDays != nil && *input.ArchiveInactiveChannelsDays < 1 {
		return fmt.Errorf("archive_inactive_channels_days must be at least 1")
	}
	if input.PurgeAfterDays != nil {
		if err := validatePurgeAfterDays(entityType, *input.PurgeAfterDays); err != nil {
			return err
		}
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE archive_policies SET
			retention_days = COALESCE($1, retention_days),
			auto_archive = COALESCE($2, auto_archive),
			archive_completed_tasks = COALESCE($3, archive_completed_tasks),
			archive_inactive_channels_days = COALESCE($4, archive_inactive_channels_days),
			compress_attachments = COALESCE($5, compress_attachments),
			purge_after_days = COALESCE($6, purge_after_days),
			updated_at = NOW()
		WHERE entity_type = $7
	`, input.RetentionDays, input.AutoArchive, input.ArchiveCompletedTasks, input.ArchiveInactiveChannelsDays, input.CompressAttachments, input.PurgeAfterDays, entityType)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateArchivePolicy Failed err: %+v", err)
		return err
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("%w for entity type: %s", ErrPolicyNotFound, entityType)
	}

	return nil
}

// RunArchiveJob triggers a manual archive job for the specified entity type.
func RunArchiveJob(ctx context.Context, entityType string, triggeredBy *uuid.UUID) (string, error) {
	if !validEntityTypes[entityType] {
		return "", fmt.Errorf("invalid entity type: %s", entityType)
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// DB-level guard: prevent overlapping archive runs across instances.
	var runningExists bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT EXISTS(SELECT 1 FROM archive_jobs WHERE entity_type = $1 AND status = 'running')`, entityType).Scan(&runningExists)
	if err != nil {
		return "", fmt.Errorf("failed to check running jobs: %w", err)
	}
	if runningExists {
		return "", &ArchiveAlreadyRunningError{EntityType: entityType}
	}

	var policy ArchivePolicy
	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT id, entity_type, retention_days, auto_archive, archive_completed_tasks, archive_inactive_channels_days, compress_attachments, purge_after_days, purged_count, purged_bytes, last_purged_at, created_at, updated_at
		FROM archive_policies WHERE entity_type = $1`, entityType).
		Scan(&policy.Id, &policy.EntityType, &policy.RetentionDays, &policy.AutoArchive, &policy.ArchiveCompletedTasks, &policy.ArchiveInactiveChannelsDays, &policy.CompressAttachments, &policy.PurgeAfterDays, &policy.PurgedCount, &policy.PurgedBytes, &policy.LastPurgedAt, &policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		return "", fmt.Errorf("policy not found for entity type: %s", entityType)
	}

	jobId := uuid.New()
	now := time.Now()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO archive_jobs (id, entity_type, status, started_at, triggered_by, created_at)
		VALUES ($1, $2, 'running', $3, $4, $3)
	`, jobId, entityType, now, triggeredBy)
	if err != nil {
		return "", fmt.Errorf("failed to create archive job: %w", err)
	}

	// Notify admin clients that a new job has started.
	go mqttBusiness.PublishArchiveJobStatus(&mqttStruct.MqttArchiveJobStatus{
		JobId:      jobId.String(),
		EntityType: entityType,
		Status:     "running",
	})

	// Run the archive process asynchronously.
	go func() {
		executeArchiveJob(jobId, entityType, &policy)
	}()

	return jobId.String(), nil
}

// entityOps bundles the archive, restore and Dgraph-sync operations for a single entity type.
//
// All "Bulk*" closures take a slice of UUID strings; both archive and
// restore paths feed the same slices (no UUID parsing) so the closures
// can be the same function pointer used during archive.
//
// OpenSearchArchiveFn / OpenSearchRestoreFn fan out to the global
// search index. They run in goroutines from RestoreItems and the
// archive paths to keep the request-handler latency bounded.
type entityOps struct {
	Label               string
	ArchiveFn           func(ctx context.Context, cutoff time.Time, policy *ArchivePolicy) (archived int64, attempted int64, ids []string, err error)
	RestoreDgraphFn     func(ctx context.Context, ids []string) error
	ArchiveDgraphFn     func(ctx context.Context, ids []string) error
	OpenSearchArchiveFn func(ids []string)
	OpenSearchRestoreFn func(ids []string)
	// MemorySourceType is the workspace-memory source_type whose captured
	// items must follow this entity's archive/restore (empty = no memory
	// link, e.g. attachments/recordings). Posts->"post", chats->"chat".
	MemorySourceType string
}

// makeOpenSearchArchive builds the closure that fans an archive event
// out to OpenSearch. The cascadeFields map declares which OpenSearch
// fields receive the deletion (e.g. {"post_id":..., "comment_post_id":...})
// so a post archive cascades to its comments and attachments.
func makeOpenSearchArchive(cascadeFields []string, indexes []string) func(ids []string) {
	return func(ids []string) {
		if len(ids) == 0 {
			return
		}
		fields := map[string][]string{}
		for _, f := range cascadeFields {
			fields[f] = ids
		}
		now := time.Now().Unix()
		go func(payload map[string][]string) {
			defer recoverOpenSearchPanic("archive cascade")
			globalSearchDomain.SyncCascadingDeletionInOpenSearchCombined(
				payload, now, indexes, "cascade",
			)
		}(fields)
	}
}

// makeOpenSearchRestore is the un-archive twin of makeOpenSearchArchive.
func makeOpenSearchRestore(cascadeFields []string, indexes []string) func(ids []string) {
	return func(ids []string) {
		if len(ids) == 0 {
			return
		}
		fields := map[string][]string{}
		for _, f := range cascadeFields {
			fields[f] = ids
		}
		go func(payload map[string][]string) {
			defer recoverOpenSearchPanic("restore cascade")
			globalSearchDomain.SyncCascadingUnarchiveInOpenSearchCombined(
				payload, indexes, "cascade",
			)
		}(fields)
	}
}

// makeOpenSearchArchiveSingle is the single-field variant for
// attachments/recordings where there's no cascade table mapping.
func makeOpenSearchArchiveSingle(field string, indexes []string) func(ids []string) {
	return func(ids []string) {
		if len(ids) == 0 {
			return
		}
		now := time.Now().Unix()
		go func(field string, ids []string) {
			defer recoverOpenSearchPanic("archive single cascade")
			globalSearchDomain.SyncCascadingDeletionInOpenSearchMulti(
				field, ids, now, indexes, "cascade",
			)
		}(field, ids)
	}
}

// makeOpenSearchRestoreSingle is the single-field unarchive twin.
func makeOpenSearchRestoreSingle(field string, indexes []string) func(ids []string) {
	return func(ids []string) {
		if len(ids) == 0 {
			return
		}
		go func(field string, ids []string) {
			defer recoverOpenSearchPanic("restore single cascade")
			globalSearchDomain.SyncCascadingUnarchiveInOpenSearchMulti(
				field, ids, indexes, "cascade",
			)
		}(field, ids)
	}
}

// recoverOpenSearchPanic is the standard recover guard for OpenSearch
// cascade goroutines. The cascade is best-effort; a panic must not
// take down the worker.
func recoverOpenSearchPanic(label string) {
	if r := recover(); r != nil {
		helpers.MessageLogs.ErrorLog.Printf("panic in %s OpenSearch cascade: %v", label, r)
	}
}

// entityRegistry maps entity type strings to their operational closures.
var entityRegistry = map[string]entityOps{
	"posts": {
		Label: "Post",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			return archivePosts(ctx, cutoff)
		},
		RestoreDgraphFn:     postDomain.BulkRestorePostsInDgraph,
		ArchiveDgraphFn:     postDomain.BulkArchivePostsInDgraph,
		OpenSearchArchiveFn: makeOpenSearchArchive([]string{"post_id", "comment_post_id", "attachment_post_id", "content_uuid", "post_uuid"}, []string{"posts", "comments", "attachments", "ai_embeddings"}),
		OpenSearchRestoreFn: makeOpenSearchRestore([]string{"post_id", "comment_post_id", "attachment_post_id", "content_uuid", "post_uuid"}, []string{"posts", "comments", "attachments", "ai_embeddings"}),
		MemorySourceType:    "post",
	},
	"chats": {
		Label: "Chat",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			return archiveChats(ctx, cutoff)
		},
		RestoreDgraphFn:     chatDomain.BulkRestoreChatsInDgraph,
		ArchiveDgraphFn:     chatDomain.BulkArchiveChatsInDgraph,
		OpenSearchArchiveFn: makeOpenSearchArchive([]string{"chat_id", "comment_chat_id", "attachment_chat_id", "content_uuid", "chat_uuid"}, []string{"chats", "comments", "attachments", "ai_embeddings"}),
		OpenSearchRestoreFn: makeOpenSearchRestore([]string{"chat_id", "comment_chat_id", "attachment_chat_id", "content_uuid", "chat_uuid"}, []string{"chats", "comments", "attachments", "ai_embeddings"}),
		MemorySourceType:    "chat",
	},
	"tasks": {
		Label: "Task",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, policy *ArchivePolicy) (int64, int64, []string, error) {
			return archiveTasks(ctx, cutoff, policy.ArchiveCompletedTasks)
		},
		RestoreDgraphFn:     taskDomain.BulkRestoreTasksInDgraph,
		ArchiveDgraphFn:     taskDomain.BulkArchiveTasksInDgraph,
		OpenSearchArchiveFn: makeOpenSearchArchive([]string{"task_id", "comment_task_id", "attachment_task_id", "content_uuid", "task_uuid"}, []string{"tasks", "comments", "attachments", "ai_embeddings"}),
		OpenSearchRestoreFn: makeOpenSearchRestore([]string{"task_id", "comment_task_id", "attachment_task_id"}, []string{"tasks", "comments", "attachments"}),
	},
	"attachments": {
		Label: "Attachment",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			return archiveAttachments(ctx, cutoff)
		},
		RestoreDgraphFn:     attachmentDomain.BulkRestoreAttachmentsInDgraph,
		ArchiveDgraphFn:     attachmentDomain.BulkArchiveAttachmentsInDgraph,
		OpenSearchArchiveFn: makeOpenSearchArchiveSingle("attachment_id", []string{"attachments"}),
		OpenSearchRestoreFn: makeOpenSearchRestoreSingle("attachment_id", []string{"attachments"}),
	},
	"recordings": {
		Label: "Recording",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			n, err := archiveRecordings(ctx, cutoff)
			return n, n, nil, err
		},
		OpenSearchRestoreFn: makeOpenSearchRestoreSingle("recording_egress_id", []string{"recordings"}),
	},
	"docs": {
		Label: "Document",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			n, err := archiveDocs(ctx, cutoff)
			return n, n, nil, err
		},
		OpenSearchRestoreFn: makeOpenSearchRestore([]string{"doc_id", "comment_doc_id", "attachment_doc_id", "content_uuid"}, []string{"docs", "comments", "attachments", "ai_embeddings"}),
	},
}

// executeArchiveJob runs the actual archiving logic.
func executeArchiveJob(jobId uuid.UUID, entityType string, policy *ArchivePolicy) {
	ctx := context.Background()
	cutoff := time.Now().AddDate(0, 0, -policy.RetentionDays)

	ops, ok := entityRegistry[entityType]
	if !ok {
		helpers.LogErrorWithContext(ctx, "executeArchiveJob: unsupported entity type %s", entityType)
		finalizeJob(ctx, jobId, entityType, 0, 0, nil, fmt.Errorf("unsupported entity type: %s", entityType))
		return
	}

	archived, attempted, ids, archiveErr := ops.ArchiveFn(ctx, cutoff, policy)

	// Sync Dgraph for entities that support it.
	if archiveErr == nil && len(ids) > 0 && ops.ArchiveDgraphFn != nil {
		if dgraphErr := ops.ArchiveDgraphFn(ctx, ids); dgraphErr != nil {
			helpers.LogErrorWithContext(ctx, "executeArchiveJob Dgraph archive sync failed for %s: %v", entityType, dgraphErr)
			// Non-fatal: job succeeds but Dgraph is out of sync.
		}
	}

	// Fan out to OpenSearch via the registry so each entity's
	// cascade fields stay declarative. The closure handles its own
	// goroutine + recover.
	if archiveErr == nil && len(ids) > 0 && ops.OpenSearchArchiveFn != nil {
		ops.OpenSearchArchiveFn(ids)
	}

	// Cascade into the Workspace Memory layer for entities that can carry
	// captured facts (posts/chats). Archived content must stop surfacing as
	// AI-queryable memory; this is reversible via the restore path's
	// RestoreMemoryBySources. Runs in its own goroutine so a slow memory
	// store never extends the archive job's critical path.
	if archiveErr == nil && len(ids) > 0 && ops.MemorySourceType != "" {
		go func(sourceType string, sourceIDs []string) {
			defer recoverOpenSearchPanic("archive memory cascade")
			ai.ArchiveMemoryBySources(context.Background(), sourceType, sourceIDs)
		}(ops.MemorySourceType, ids)
	}

	finalizeJob(ctx, jobId, entityType, attempted, archived, ids, archiveErr)

	var finalStatus string
	if archiveErr != nil {
		finalStatus = "failed"
	} else {
		finalStatus = "completed"
	}
	helpers.MessageLogs.InfoLog.Printf("Archive job %s finished: type=%s attempted=%d archived=%d failed=%d status=%s",
		jobId.String(), entityType, attempted, archived, int(attempted-archived), finalStatus)
}

func finalizeJob(ctx context.Context, jobId uuid.UUID, entityType string, attempted, archived int64, ids []string, jobErr error) {
	status := "completed"
	var errMsg *string
	itemsFailed := int(attempted - archived)
	if jobErr != nil {
		status = "failed"
		e := jobErr.Error()
		errMsg = &e
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Persist the per-item id list in archive_job_items rather than a
	// JSONB blob on archive_jobs. Done in chunks of 1000 inside a
	// single transaction so a 100k-item job doesn't issue 100k inserts
	// or a >1MB single statement.
	if err := persistArchiveJobItems(dbCtx, jobId, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "finalizeJob: failed to persist job items: %v", err)
	}

	_, execErr := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE archive_jobs SET status=$1, completed_at=$2, items_processed=$3, items_archived=$4, items_failed=$5, error_message=$6, metadata=NULL WHERE id=$7
	`, status, time.Now(), attempted, archived, itemsFailed, errMsg, jobId)
	if execErr != nil {
		helpers.LogErrorWithContext(ctx, "finalizeJob: failed to finalize job status: %v", execErr)
	}

	// Notify admin clients that the job has reached terminal state.
	mqttPayload := &mqttStruct.MqttArchiveJobStatus{
		JobId:          jobId.String(),
		EntityType:     entityType,
		Status:         status,
		ItemsProcessed: int(attempted),
		ItemsArchived:  int(archived),
		ItemsFailed:    itemsFailed,
	}
	if errMsg != nil {
		mqttPayload.ErrorMessage = *errMsg
	}
	go mqttBusiness.PublishArchiveJobStatus(mqttPayload)
}

// persistArchiveJobItems writes the (job_id, entity_id) rows in
// chunks of 1000 within a single transaction. ON CONFLICT DO NOTHING
// makes the call idempotent so a transient PG hiccup that gets
// retried won't bloat the table with duplicates.
func persistArchiveJobItems(ctx context.Context, jobId uuid.UUID, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const chunkSize = 1000

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for i := 0; i < len(ids); i += chunkSize {
		end := i + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, 1+len(batch))
		args = append(args, jobId)
		for j, id := range batch {
			placeholders[j] = fmt.Sprintf("($1, $%d)", j+2)
			args = append(args, id)
		}
		query := fmt.Sprintf(
			`INSERT INTO archive_job_items (job_id, entity_id) VALUES %s ON CONFLICT DO NOTHING`,
			strings.Join(placeholders, ","),
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadArchiveJobItems returns the entity ids previously archived by a
// job. Reads from archive_job_items first; falls back to the legacy
// metadata.archived_ids JSONB blob for jobs that pre-date migration 68.
func loadArchiveJobItems(ctx context.Context, jobId uuid.UUID, legacyMetadata *string) ([]string, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx,
		`SELECT entity_id FROM archive_job_items WHERE job_id = $1`, jobId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, 256)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(out) > 0 {
		return out, nil
	}

	// Fallback for legacy rows where the ids were embedded in metadata.
	if legacyMetadata != nil && *legacyMetadata != "" {
		var meta struct {
			ArchivedIds []string `json:"archived_ids"`
		}
		if err := json.Unmarshal([]byte(*legacyMetadata), &meta); err == nil {
			return meta.ArchivedIds, nil
		}
	}
	return nil, nil
}

// archiveTable is the generic engine behind archivePosts /
// archiveChats / archiveTasks / archiveAttachments. It:
//   - lists all soft-deletable rows under cutoff
//   - issues a single UPDATE to set deleted_at=NOW()
//   - returns (rowsAffected, total, ids, err)
//
// The OpenSearch cascade is intentionally NOT fired here; callers can
// rely on the entity registry's OpenSearchArchiveFn from
// executeArchiveJob, which has a single recover guard and consistent
// shape across entity types.
//
// extraWhere appends to the WHERE clause (e.g. tasks adds status IN
// ('done','canceled')). Pass an empty string for no extra predicate.
func archiveTable(ctx context.Context, table, extraWhere string, cutoff time.Time) (rowsAffected int64, total int64, ids []string, err error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	idQuery := fmt.Sprintf(`SELECT id::text FROM %s WHERE created_at < $1 AND deleted_at IS NULL`, table)
	updateQuery := fmt.Sprintf(`UPDATE %s SET deleted_at = NOW(), updated_at = NOW() WHERE created_at < $1 AND deleted_at IS NULL`, table)
	if extraWhere != "" {
		idQuery += " AND " + extraWhere
		updateQuery += " AND " + extraWhere
	}

	rows, qErr := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, idQuery, cutoff)
	if qErr != nil {
		return 0, 0, nil, qErr
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr == nil {
			ids = append(ids, id)
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		return 0, 0, nil, iterErr
	}

	result, uErr := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, updateQuery, cutoff)
	if uErr != nil {
		return 0, int64(len(ids)), ids, uErr
	}
	rowsAffected, _ = result.RowsAffected()
	total = int64(len(ids))
	return rowsAffected, total, ids, nil
}

func archivePosts(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	return archiveTable(ctx, "posts", "", cutoff)
}

func archiveChats(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	return archiveTable(ctx, "chats", "", cutoff)
}

// archiveTaskBatch bounds one run of archiving completed tasks; the next run
// takes the rest.
const archiveTaskBatch = 5000

func archiveTasks(ctx context.Context, cutoff time.Time, archiveCompletedTasks bool) (int64, int64, []string, error) {
	if !archiveCompletedTasks {
		return archiveTable(ctx, "tasks", "", cutoff)
	}
	// Only the done and canceled ones, closed before the cutoff. Their status
	// is in the graph: Postgres has no status column, so this asked it for one
	// and, with the setting on (the default), every task archive failed.
	closed, err := taskDomain.ClosedTaskUUIDsBefore(ctx, cutoff, archiveTaskBatch)
	if err != nil || len(closed) == 0 {
		return 0, 0, nil, err
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx,
		`UPDATE tasks SET deleted_at = NOW(), updated_at = NOW()
		 WHERE id = ANY($2::uuid[]) AND created_at < $1 AND deleted_at IS NULL
		 RETURNING id::text`, cutoff, pq.Array(closed))
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	return int64(len(ids)), int64(len(ids)), ids, nil
}

// archiveAttachments writes deleted_at without updating updated_at
// (the column is missing on the attachments table). Falls back to the
// generic helper but tolerates the missing column in the UPDATE.
func archiveAttachments(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx,
		`SELECT id::text FROM attachments WHERE created_at < $1 AND deleted_at IS NULL`, cutoff)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}

	result, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE attachments SET deleted_at = NOW() WHERE created_at < $1 AND deleted_at IS NULL`, cutoff)
	if err != nil {
		return 0, int64(len(ids)), ids, err
	}
	rowsAffected, _ := result.RowsAffected()
	return rowsAffected, int64(len(ids)), ids, nil
}

func archiveRecordings(ctx context.Context, cutoff time.Time) (int64, error) {
	egressIds, err := recordingDomain.GetRecordingEgressIdsOlderThan(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(egressIds) == 0 {
		return 0, nil
	}

	if err := recordingDomain.BulkArchiveRecordings(ctx, egressIds); err != nil {
		return 0, err
	}
	return int64(len(egressIds)), nil
}

func archiveDocs(ctx context.Context, cutoff time.Time) (int64, error) {
	docUuids, err := docDomain.GetDocUuidsOlderThan(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(docUuids) == 0 {
		return 0, nil
	}

	if err := docDomain.BulkArchiveDocs(ctx, docUuids); err != nil {
		return 0, err
	}
	return int64(len(docUuids)), nil
}

// GetArchiveJobs returns recent archive job history.
func GetArchiveJobs(ctx context.Context) ([]*ArchiveJob, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, entity_type, status, started_at, completed_at, items_processed, items_archived, items_failed, error_message, triggered_by, created_at
		FROM archive_jobs
		ORDER BY created_at DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*ArchiveJob
	for rows.Next() {
		var j ArchiveJob
		if err := rows.Scan(&j.Id, &j.EntityType, &j.Status, &j.StartedAt, &j.CompletedAt, &j.ItemsProcessed, &j.ItemsArchived, &j.ItemsFailed, &j.ErrorMessage, &j.TriggeredBy, &j.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return jobs, nil
}

// GetArchiveStats returns statistics about archived data.
func GetArchiveStats(ctx context.Context) (*ArchiveStats, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	stats := &ArchiveStats{}

	// Consolidated count: one round-trip for all four Postgres tables.
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT 'posts' AS entity, COUNT(*) AS cnt FROM posts WHERE deleted_at IS NOT NULL
		UNION ALL
		SELECT 'chats', COUNT(*) FROM chats WHERE deleted_at IS NOT NULL
		UNION ALL
		SELECT 'tasks', COUNT(*) FROM tasks WHERE deleted_at IS NOT NULL
		UNION ALL
		SELECT 'attachments', COUNT(*) FROM attachments WHERE deleted_at IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("GetArchiveStats postgres count failed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entity string
		var cnt int64
		if err := rows.Scan(&entity, &cnt); err != nil {
			continue
		}
		switch entity {
		case "posts":
			stats.TotalArchivedPosts = cnt
		case "chats":
			stats.TotalArchivedChats = cnt
		case "tasks":
			stats.TotalArchivedTasks = cnt
		case "attachments":
			stats.TotalArchivedAttachments = cnt
		}
	}

	// Dgraph counts for docs and recordings (soft-deleted = gt(deleted_at, sentinel)).
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	stats.TotalArchivedDocs, _ = docDomain.CountRecentlyArchivedDocs(ctx, cutoff)
	stats.TotalArchivedRecordings, _ = recordingDomain.CountRecentlyArchivedRecordings(ctx, cutoff)

	return stats, nil
}

// checkRunningArchiveJob returns an error if an archive job is already running for the given entity type.
func checkRunningArchiveJob(ctx context.Context, entityType string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var runningExists bool
	checkErr := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT EXISTS(SELECT 1 FROM archive_jobs WHERE entity_type = $1 AND status = 'running')`, entityType).Scan(&runningExists)
	if checkErr != nil {
		return fmt.Errorf("failed to check running jobs: %w", checkErr)
	}
	if runningExists {
		return &ArchiveAlreadyRunningError{EntityType: entityType}
	}
	return nil
}

// RestoreItems restores archived items by clearing their deleted_at timestamp.
// Dgraph sync is performed synchronously to match the archive path; OpenSearch
// cascades are async fire-and-forget.
//
// The implementation routes through entityRegistry so adding a new
// entity type is one registry entry instead of touching this function.
// Recordings and Docs use dedicated paths because their data lives in
// Dgraph rather than Postgres.
func RestoreItems(ctx context.Context, entityType string, entityIds []uuid.UUID) (int, error) {
	if len(entityIds) == 0 {
		return 0, nil
	}

	// Guard: prevent restore while an archive job is running for the same entity type.
	if err := checkRunningArchiveJob(ctx, entityType); err != nil {
		return 0, err
	}

	// Recordings and docs aren't backed by Postgres soft-delete; they
	// have their own restore paths.
	switch entityType {
	case "docs":
		return restoreDocs(ctx, entityIds)
	case "recordings":
		return restoreRecordings(ctx, entityIds)
	}

	ops, registered := entityRegistry[entityType]
	if !registered {
		return 0, fmt.Errorf("unsupported entity type for restore: %s", entityType)
	}

	queryTemplate, ok := entityRestoreQuery[entityType]
	if !ok {
		return 0, fmt.Errorf("unsupported entity type for restore: %s", entityType)
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	placeholders := make([]string, len(entityIds))
	args := make([]interface{}, len(entityIds))
	for i, id := range entityIds {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id.String()
	}
	query := fmt.Sprintf(queryTemplate, strings.Join(placeholders, ","))

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreItems Failed to bulk restore %s err: %+v", entityType, err)
		return 0, err
	}
	defer rows.Close()

	restored := make([]string, 0, len(entityIds))
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			continue
		}
		restored = append(restored, idStr)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreItems error iterating results: %v", err)
	}

	if len(restored) == 0 {
		return 0, nil
	}

	// Synchronous Dgraph restore (matches the archive path behaviour).
	if ops.RestoreDgraphFn != nil {
		if dgErr := ops.RestoreDgraphFn(ctx, restored); dgErr != nil {
			helpers.LogErrorWithContext(ctx,
				"RestoreItems Dgraph restore error for %s: %v", entityType, dgErr)
		}
	}

	// Async OpenSearch cascade.
	if ops.OpenSearchRestoreFn != nil {
		ops.OpenSearchRestoreFn(restored)
	}

	// Revive the Workspace Memory items the archive cascade removed for
	// these sources (posts/chats). The reason marker ensures we never
	// resurrect memory a user independently hard-deleted. Own goroutine so
	// the restore request isn't coupled to the memory store.
	if ops.MemorySourceType != "" {
		go func(sourceType string, sourceIDs []string) {
			defer recoverOpenSearchPanic("restore memory cascade")
			ai.RestoreMemoryBySources(context.Background(), sourceType, sourceIDs)
		}(ops.MemorySourceType, restored)
	}

	return len(restored), nil
}

// UndoArchiveJob reverses an archive job by restoring the exact items that were archived,
// UndoArchiveJob reverses an archive job by restoring the exact items
// that were archived. Looks up the items in archive_job_items first
// and falls back to the legacy metadata.archived_ids JSONB blob for
// jobs that ran before migration 68.
func UndoArchiveJob(ctx context.Context, jobId uuid.UUID) (int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var job struct {
		EntityType string
		Status     string
		Metadata   *string
	}
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT entity_type, status, metadata FROM archive_jobs WHERE id = $1`, jobId).
		Scan(&job.EntityType, &job.Status, &job.Metadata)
	if err != nil {
		return 0, fmt.Errorf("archive job not found: %w", err)
	}
	if job.Status != "completed" {
		return 0, fmt.Errorf("archive job is not in completed state")
	}
	if !entitySupportsUndo[job.EntityType] {
		return 0, fmt.Errorf("undo not supported for entity type: %s", job.EntityType)
	}

	idStrs, err := loadArchiveJobItems(ctx, jobId, job.Metadata)
	if err != nil {
		return 0, fmt.Errorf("failed to load archive job items: %w", err)
	}
	if len(idStrs) == 0 {
		return 0, fmt.Errorf("no archived item ids found for job (was this job archived before exact-id tracking was enabled?)")
	}

	ids := make([]uuid.UUID, 0, len(idStrs))
	for _, idStr := range idStrs {
		if id, parseErr := uuid.Parse(idStr); parseErr == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("no valid archived item ids found")
	}

	count, err := RestoreItems(ctx, job.EntityType, ids)
	if err != nil {
		return 0, err
	}

	_, execErr := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE archive_jobs SET status = 'cancelled', metadata = jsonb_build_object('restored', true) WHERE id = $1`, jobId)
	if execErr != nil {
		helpers.LogErrorWithContext(ctx, "UndoArchiveJob: failed to mark job cancelled: %v", execErr)
	}

	return count, nil
}

// ArchivedItem represents a single archived item for the restore UI.
type ArchivedItem struct {
	Id         string    `json:"id"`
	Name       string    `json:"name"`
	ArchivedAt time.Time `json:"archived_at"`
}

// GetRecentlyArchivedItems returns items archived in the last 30 days for browsing.
func GetRecentlyArchivedItems(ctx context.Context, entityType string, limit int, offset int) ([]*ArchivedItem, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	label, ok := entityLabels[entityType]
	if !ok {
		return nil, 0, fmt.Errorf("unsupported entity type: %s", entityType)
	}

	switch entityType {
	case "posts", "chats", "tasks", "attachments":
		// supported
	case "docs":
		return getRecentlyArchivedDocs(ctx, limit, offset, label)
	case "recordings":
		return getRecentlyArchivedRecordings(ctx, limit, offset, label)
	default:
		return nil, 0, fmt.Errorf("unsupported entity type: %s", entityType)
	}

	countQuery, ok := entityRecentCountQuery[entityType]
	if !ok {
		return nil, 0, fmt.Errorf("unsupported entity type: %s", entityType)
	}
	listQuery, ok := entityRecentListQuery[entityType]
	if !ok {
		return nil, 0, fmt.Errorf("unsupported entity type: %s", entityType)
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	cutoff := time.Now().AddDate(0, 0, -30)
	var total int64
	postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, countQuery, cutoff).Scan(&total)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, listQuery, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []*ArchivedItem
	for rows.Next() {
		var item ArchivedItem
		if err := rows.Scan(&item.Id, &item.ArchivedAt); err != nil {
			continue
		}
		item.Name = fmt.Sprintf("%s · %s (%s)", label, item.ArchivedAt.Format("Jan 02, 3:04 PM"), item.Id[:8])
		items = append(items, &item)
	}

	return items, total, nil
}

func getRecentlyArchivedDocs(ctx context.Context, limit, offset int, label string) ([]*ArchivedItem, int64, error) {
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	docs, total, err := docDomain.GetRecentlyArchivedDocs(ctx, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	items := make([]*ArchivedItem, 0, len(docs))
	for _, d := range docs {
		if d.Uuid == "" || d.DeletedAt == nil {
			continue
		}
		items = append(items, &ArchivedItem{
			Id:         d.Uuid,
			Name:       fmt.Sprintf("%s · %s (%s)", label, d.DeletedAt.Format("Jan 02, 3:04 PM"), d.Uuid[:8]),
			ArchivedAt: *d.DeletedAt,
		})
	}
	return items, total, nil
}

func getRecentlyArchivedRecordings(ctx context.Context, limit, offset int, label string) ([]*ArchivedItem, int64, error) {
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	recs, total, err := recordingDomain.GetRecentlyArchivedRecordings(ctx, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	items := make([]*ArchivedItem, 0, len(recs))
	for _, r := range recs {
		if r.EgressId == "" || r.DeletedAt == nil {
			continue
		}
		items = append(items, &ArchivedItem{
			Id:         r.EgressId,
			Name:       fmt.Sprintf("%s · %s (%s)", label, r.DeletedAt.Format("Jan 02, 3:04 PM"), r.EgressId[:8]),
			ArchivedAt: *r.DeletedAt,
		})
	}
	return items, total, nil
}

func restoreRecordings(ctx context.Context, entityIds []uuid.UUID) (int, error) {
	egressIds := make([]string, len(entityIds))
	for i, id := range entityIds {
		egressIds[i] = id.String()
	}
	// Synchronous data-path restore.
	if err := recordingDomain.BulkRestoreRecordings(ctx, egressIds); err != nil {
		return 0, err
	}
	// OpenSearch cascade async.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf("panic in restoreRecordings OpenSearch cascade: %v", r)
			}
		}()
		globalSearchDomain.SyncCascadingUnarchiveInOpenSearchMulti(
			"recording_egress_id", egressIds, []string{"recordings"}, "cascade",
		)
	}()
	return len(entityIds), nil
}

func restoreDocs(ctx context.Context, entityIds []uuid.UUID) (int, error) {
	docUuids := make([]string, len(entityIds))
	for i, id := range entityIds {
		docUuids[i] = id.String()
	}
	// Synchronous data-path restore.
	if err := docDomain.BulkRestoreDocs(ctx, docUuids); err != nil {
		return 0, err
	}
	// OpenSearch cascade async.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf("panic in restoreDocs OpenSearch cascade: %v", r)
			}
		}()
		globalSearchDomain.SyncCascadingUnarchiveInOpenSearchCombined(
			map[string][]string{
				"doc_id":            docUuids,
				"comment_doc_id":    docUuids,
				"attachment_doc_id": docUuids,
				"content_uuid":      docUuids,
			},
			[]string{"docs", "comments", "attachments", "ai_embeddings"},
			"cascade",
		)
	}()
	return len(entityIds), nil
}

// StartAutoArchiver runs a background scheduler that executes archive jobs
// based on policies where auto_archive is true. Runs every hour after a 5-minute initial delay.
// The provided context is used for graceful shutdown.
func StartAutoArchiver(ctx context.Context) {
	go func() {
		time.Sleep(5 * time.Minute)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Auto-archiver shutting down")
				return
			case <-ticker.C:
				policies, err := GetArchivePolicies(ctx)
				if err != nil {
					helpers.LogErrorWithContext(ctx, "StartAutoArchiver failed to fetch policies: %v", err)
				} else {
					for _, policy := range policies {
						if policy.AutoArchive {
							helpers.MessageLogs.InfoLog.Printf("Auto-Archiver executing policy for %s", policy.EntityType)
							_, err := RunArchiveJob(ctx, policy.EntityType, nil)
							if err != nil {
								helpers.LogErrorWithContext(ctx, "StartAutoArchiver failed for %s: %v", policy.EntityType, err)
							}
						}
					}
					// Permanent removal runs on the same tick, after archiving,
					// for every policy that asks for it. Not gated on
					// auto_archive: what was archived by hand is due too.
					RunPurges(ctx, policies, time.Now())
				}
			}
		}
	}()
}
