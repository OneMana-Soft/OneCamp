package business

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	archiveModels "github.com/akashc777/OneCamp/models/postgres/Archive"
	"strconv"
	"strings"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
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
	"posts": true, "chats": true, "tasks": true, "attachments": true, "docs": true,
}

// entityRestoreQuery holds the RETURNING id::text query per entity type, over
// the ids as one uuid array ($1): compared as uuids, so the primary key's
// index finds them (cast to text, they scanned the table).
// entityRecentCountQuery holds the COUNT query per entity type for recently-archived listings.
// entityRecentListQuery holds the list query per entity type for recently-archived listings.
// ArchivePolicy represents an archive policy.
// ArchivePolicy and ArchiveJob are the model's (models/postgres/Archive).
type ArchivePolicy = archiveModels.ArchivePolicy

// ArchiveJob represents an archive job execution.
type ArchiveJob = archiveModels.ArchiveJob

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
	policies, err := archiveModels.ListPolicies(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetArchivePolicies Failed err: %+v", err)
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

	rowsAffected, err := archiveModels.UpdatePolicy(ctx, entityType, input.RetentionDays, input.AutoArchive, input.ArchiveCompletedTasks,
		input.ArchiveInactiveChannelsDays, input.CompressAttachments, input.PurgeAfterDays)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateArchivePolicy Failed err: %+v", err)
		return err
	}
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

	// DB-level guard: prevent overlapping archive runs across instances.
	if err := checkRunningArchiveJob(ctx, entityType); err != nil {
		return "", err
	}

	policy, found, err := archiveModels.GetPolicy(ctx, entityType)
	if err != nil || !found {
		return "", fmt.Errorf("policy not found for entity type: %s", entityType)
	}

	jobId := uuid.New()
	now := time.Now()

	if err := archiveModels.CreateJob(ctx, jobId, entityType, now, triggeredBy); err != nil {
		return "", fmt.Errorf("failed to create archive job: %w", err)
	}

	// Notify admin clients that a new job has started.
	go mqttBusiness.PublishArchiveJobStatus(&mqttStruct.MqttArchiveJobStatus{
		JobId:      jobId.String(),
		EntityType: entityType,
		Status:     "running",
	})

	// Run the archive process asynchronously.
	helpers.GoSafeNamed("business/Archive/archiveBusiness:RunArchiveJob", func() {
		executeArchiveJob(jobId, entityType, policy)
	})

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
//
// ArchiveFn's ids are what it archived, and only that, even when it
// fails: docs move in batches, and a batch that fails leaves the ones
// before it archived. A run follows its ids into search either way.
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
	// Docs live in the graph, so archiveDocs writes them there itself and
	// answers which it archived (there are no Postgres rows to sync). Search
	// follows: each doc's own entry is written from the doc, and its comments',
	// files' and AI entries are cascaded (restoring clears what was cascaded).
	"docs": {
		Label: "Document",
		ArchiveFn: func(ctx context.Context, cutoff time.Time, _ *ArchivePolicy) (int64, int64, []string, error) {
			return archiveDocs(ctx, cutoff)
		},
		OpenSearchArchiveFn: docSearchFollows(makeOpenSearchArchive(docCascadeFields, docCascadeIndexes)),
		OpenSearchRestoreFn: docSearchFollows(makeOpenSearchRestore(docCascadeFields, docCascadeIndexes)),
	},
}

// docCascadeFields are the fields a doc's comments, files and AI entries name
// it by: comment_doc_id, attachment_doc_id, and in the AI index content_uuid
// (the doc's own entry) and doc_uuid (its comments'). The docs index's own
// field is doc_uuid; restore used to clear "doc_id", which no index has.
var (
	docCascadeFields  = []string{"comment_doc_id", "attachment_doc_id", "content_uuid", "doc_uuid"}
	docCascadeIndexes = []string{"comments", "attachments", "ai_embeddings"}
)

// docSearchFollows has each doc's own search entry written from the doc
// (archived or restored, whichever it now is), then runs the cascade for
// what is in it.
func docSearchFollows(cascade func(ids []string)) func(ids []string) {
	return func(ids []string) {
		for _, id := range ids {
			docDomain.SyncDocSearch(id, false)
		}
		cascade(ids)
	}
}

// executeArchiveJob runs the actual archiving logic.
func executeArchiveJob(jobId uuid.UUID, entityType string, policy *ArchivePolicy) {
	ctx := context.Background()
	cutoff := time.Now().AddDate(0, 0, -policy.RetentionDays)

	ops, ok := entityRegistry[entityType]
	if !ok {
		helpers.LogErrorWithContext(ctx, "executeArchiveJob: unsupported entity type %s", entityType)
		finishJob(ctx, jobId, entityType, 0, 0, nil, fmt.Errorf("unsupported entity type: %s", entityType))
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
	// goroutine + recover. Also when the run failed part way: ids are
	// what it archived (entityOps), and a doc run whose later batch
	// failed left the docs its earlier batches archived findable, their
	// comments and files too.
	if len(ids) > 0 && ops.OpenSearchArchiveFn != nil {
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

	finishJob(ctx, jobId, entityType, attempted, archived, ids, archiveErr)

	var finalStatus string
	if archiveErr != nil {
		finalStatus = "failed"
	} else {
		finalStatus = "completed"
	}
	helpers.MessageLogs.InfoLog.Printf("Archive job %s finished: type=%s attempted=%d archived=%d failed=%d status=%s",
		jobId.String(), entityType, attempted, archived, int(attempted-archived), finalStatus)
}

// finishJob records how a run ended (finalizeJob); a seam for tests.
var finishJob = finalizeJob

func finalizeJob(ctx context.Context, jobId uuid.UUID, entityType string, attempted, archived int64, ids []string, jobErr error) {
	status := "completed"
	var errMsg *string
	itemsFailed := int(attempted - archived)
	if jobErr != nil {
		status = "failed"
		e := jobErr.Error()
		errMsg = &e
	}

	// Persist the per-item id list in archive_job_items rather than a
	// JSONB blob on archive_jobs, in one statement.
	if err := archiveModels.AddJobItems(ctx, jobId, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "finalizeJob: failed to persist job items: %v", err)
	}

	if execErr := archiveModels.FinishJob(ctx, jobId, status, time.Now(), attempted, archived, itemsFailed, errMsg); execErr != nil {
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
	helpers.GoSafeNamed("mqttBusiness.PublishArchiveJobStatus", func() { mqttBusiness.PublishArchiveJobStatus(mqttPayload) })
}

// loadArchiveJobItems returns the entity ids previously archived by a
// job. Reads from archive_job_items first; falls back to the legacy
// metadata.archived_ids JSONB blob for jobs that pre-date migration 68.
func loadArchiveJobItems(ctx context.Context, jobId uuid.UUID, legacyMetadata *string) ([]string, error) {
	out, err := archiveModels.JobItems(ctx, jobId)
	if err != nil {
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

// archiveTable archives a table's live rows made before the cutoff, in one
// statement that answers the ids it took (models ArchiveBefore): it listed
// them and then updated by the same predicate, so a row deleted or restored
// between the two was counted, and recorded, as archived. The OpenSearch
// cascade follows from executeArchiveJob's entity registry.
func archiveTable(ctx context.Context, table string, cutoff time.Time) (rowsAffected int64, total int64, ids []string, err error) {
	ids, err = archiveModels.ArchiveBefore(ctx, table, cutoff)
	if err != nil {
		// One statement, so none of them was archived: no ids, or the run
		// would take live rows out of search (entityOps). How many it set
		// out to take is the run's failed count.
		attempted, _ := archiveModels.CountBefore(ctx, table, cutoff)
		return 0, attempted, nil, err
	}
	return int64(len(ids)), int64(len(ids)), ids, nil
}

func archivePosts(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	return archiveTable(ctx, "posts", cutoff)
}

func archiveChats(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	return archiveTable(ctx, "chats", cutoff)
}

// archiveTaskBatch bounds one run of archiving completed tasks; the next run
// takes the rest.
const archiveTaskBatch = 5000

func archiveTasks(ctx context.Context, cutoff time.Time, archiveCompletedTasks bool) (int64, int64, []string, error) {
	if !archiveCompletedTasks {
		return archiveTable(ctx, "tasks", cutoff)
	}
	// Only the done and canceled ones, closed before the cutoff. Their status
	// is in the graph: Postgres has no status column, so this asked it for one
	// and, with the setting on (the default), every task archive failed.
	closed, err := taskDomain.ClosedTaskUUIDsBefore(ctx, cutoff, archiveTaskBatch)
	if err != nil || len(closed) == 0 {
		return 0, 0, nil, err
	}

	ids, err := archiveModels.ArchiveTasksIn(ctx, cutoff, closed)
	if err != nil {
		return 0, 0, nil, err
	}
	return int64(len(ids)), int64(len(ids)), ids, nil
}

// archiveAttachments writes deleted_at without updating updated_at
// (the column is missing on the attachments table). Falls back to the
// generic helper but tolerates the missing column in the UPDATE.
func archiveAttachments(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	return archiveTable(ctx, "attachments", cutoff)
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

// archiveDocBatch bounds one run of archiving docs; the next run takes the rest.
const archiveDocBatch = 5000

// archiveDocs archives the live docs nobody has changed since the cutoff and
// answers which it archived, which the job records (so it can be undone).
// Every run used to fail: the graph was sent a query that was not valid DQL.
func archiveDocs(ctx context.Context, cutoff time.Time) (int64, int64, []string, error) {
	due, err := docDomain.GetDocsInactiveSince(ctx, cutoff, archiveDocBatch)
	if err != nil || len(due) == 0 {
		return 0, 0, nil, err
	}
	// Whole subtrees only: never a page under one still in use, nor one with a
	// subpage still in use (docTree.go).
	taken, err := archivableDocsOf(ctx, due)
	if err != nil || len(taken) == 0 {
		return 0, int64(len(due)), nil, err
	}
	archived, err := docDomain.BulkArchiveDocs(ctx, taken, cutoff)
	return int64(len(archived)), int64(len(due)), archived, err
}

// GetArchiveJobs returns recent archive job history.
func GetArchiveJobs(ctx context.Context) ([]*ArchiveJob, error) {
	return archiveModels.ListJobs(ctx)
}

// GetArchiveStats returns statistics about archived data.
func GetArchiveStats(ctx context.Context) (*ArchiveStats, error) {
	stats := &ArchiveStats{}

	// Consolidated count: one round-trip for all four Postgres tables.
	counts, err := archiveModels.ArchivedCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetArchiveStats postgres count failed: %w", err)
	}
	stats.TotalArchivedPosts = counts["posts"]
	stats.TotalArchivedChats = counts["chats"]
	stats.TotalArchivedTasks = counts["tasks"]
	stats.TotalArchivedAttachments = counts["attachments"]

	// Dgraph counts for docs and recordings (soft-deleted = gt(deleted_at, sentinel)).
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	stats.TotalArchivedDocs, _ = docDomain.CountRecentlyArchivedDocs(ctx, cutoff)
	stats.TotalArchivedRecordings, _ = recordingDomain.CountRecentlyArchivedRecordings(ctx, cutoff)

	return stats, nil
}

// checkRunningArchiveJob returns an error if an archive job is already running for the given entity type.
func checkRunningArchiveJob(ctx context.Context, entityType string) error {
	running, err := archiveModels.RunningJobExists(ctx, entityType)
	if err != nil {
		return fmt.Errorf("failed to check running jobs: %w", err)
	}
	if running {
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
		// Any deleted doc, archived or deleted by its owner: there is no
		// Trash in the app yet, so this list is how a deleted doc comes back.
		return restoreDocs(ctx, entityIds, false)
	case "recordings":
		return restoreRecordings(ctx, entityIds)
	}

	ops, registered := entityRegistry[entityType]
	if !registered {
		return 0, fmt.Errorf("unsupported entity type for restore: %s", entityType)
	}

	if !archiveModels.Archivable(entityType) {
		return 0, fmt.Errorf("unsupported entity type for restore: %s", entityType)
	}

	ids := make([]string, len(entityIds))
	for i, id := range entityIds {
		ids[i] = id.String()
	}

	restored, err := archiveModels.Restore(ctx, entityType, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreItems Failed to bulk restore %s err: %+v", entityType, err)
		return 0, err
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
	var job struct {
		EntityType string
		Status     string
		Metadata   *string
	}
	var found bool
	var err error
	job.EntityType, job.Status, job.Metadata, found, err = archiveModels.UndoJob(ctx, jobId)
	if err == nil && !found {
		err = sql.ErrNoRows
	}
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

	var count int
	if job.EntityType == "docs" {
		// Only the docs this run archived that are archived still: a doc its
		// owner deleted since stays deleted.
		if err := checkRunningArchiveJob(ctx, job.EntityType); err != nil {
			return 0, err
		}
		count, err = restoreDocs(ctx, ids, true)
	} else {
		count, err = RestoreItems(ctx, job.EntityType, ids)
	}
	if err != nil {
		return 0, err
	}

	if execErr := archiveModels.MarkJobUndone(ctx, jobId); execErr != nil {
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

	cutoff := time.Now().AddDate(0, 0, -30)
	rows, total, err := archiveModels.RecentlyArchived(ctx, entityType, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}

	var items []*ArchivedItem
	for _, r := range rows {
		item := ArchivedItem{Id: r.ID, ArchivedAt: r.At}
		item.Name = fmt.Sprintf("%s · %s (%s)", label, item.ArchivedAt.Format("Jan 02, 3:04 PM"), item.Id[:8])
		items = append(items, &item)
	}

	return items, total, nil
}

// The restore list's reads of docs: production wiring, which tests replace.
var (
	recentlyDeletedDocs = docDomain.GetRecentlyArchivedDocs
	docsCouldRead       = docDomain.DocsCouldReadFor
)

// getRecentlyArchivedDocs lists the docs deleted in the last 30 days for the
// admin asking (the request's user, in ctx). A private doc they may not read
// shows as "Private doc" with its owner, never its title: an admin restores
// docs, which doesn't make every doc theirs to read. Decided by the doc read
// rule with the deletion set aside (domain/Doc CouldRead, which business/Doc
// decides by; this package can't import business/Doc, which depends on it).
// A doc the rule can't be asked about (the graph didn't answer, or no one is
// asking) shows as private too.
func getRecentlyArchivedDocs(ctx context.Context, limit, offset int, label string) ([]*ArchivedItem, int64, error) {
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	docs, total, err := recentlyDeletedDocs(ctx, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if d != nil && d.Uuid != "" {
			ids = append(ids, d.Uuid)
		}
	}
	readable := map[string]bool{}
	if viewer, _ := askingUser(ctx); viewer != "" && len(ids) > 0 {
		if got, rerr := docsCouldRead(ctx, ids, viewer); rerr != nil {
			helpers.LogErrorWithContext(ctx, "business/getRecentlyArchivedDocs could not ask who may read the docs, so no title is shown: %v", rerr)
		} else {
			readable = got
		}
	}
	items := make([]*ArchivedItem, 0, len(docs))
	for _, d := range docs {
		if d == nil || d.Uuid == "" || !helpers.IsSoftDeleted(d.DeletedAt) {
			continue
		}
		items = append(items, &ArchivedItem{
			Id:         d.Uuid,
			Name:       docRestoreName(d, readable[strings.ToLower(d.Uuid)]),
			ArchivedAt: *d.DeletedAt,
		})
	}
	return items, total, nil
}

// askingUser is who a request is for: their id and the name others see them
// by, "" for no one.
func askingUser(ctx context.Context) (id, name string) {
	u, ok := userModel.FromContext(ctx)
	if !ok || u == nil {
		return "", ""
	}
	id = u.UserDgraphInfo.Uuid
	if id == "" && u.UserPostgresInfo.Id != uuid.Nil {
		id = u.UserPostgresInfo.Id.String()
	}
	return id, u.UserDgraphInfo.DisplayName()
}

// docRestoreName is how a doc reads in the restore list: its title, and, for
// one its owner deleted rather than one the retention archived, who deleted
// it, so an admin bringing it back knows they are undoing someone's choice.
// The list named every doc "Document · <time> (<id>)", and mixed docs owners
// deleted in with archived ones. A doc the admin may not read (readable
// false) is "Private doc" and whose it is, never its title; the list's time
// says when it went. Pure.
func docRestoreName(d *dgraphStruct.DgraphDoc, readable bool) string {
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = "Untitled document"
	}
	if !readable {
		title = "Private doc"
	}
	owner := d.CreatedBy.DisplayName()
	if helpers.IsSoftDeleted(d.ArchivedAt) {
		if !readable && owner != "" {
			return title + " · owned by " + owner
		}
		return title
	}
	if owner == "" {
		owner = "its owner"
	}
	return title + " · deleted by " + owner
}

// recentlyArchivedRecordings is the restore list's read of recordings:
// production wiring, which tests replace.
var recentlyArchivedRecordings = recordingDomain.GetRecentlyArchivedRecordings

func getRecentlyArchivedRecordings(ctx context.Context, limit, offset int, label string) ([]*ArchivedItem, int64, error) {
	cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	recs, total, err := recentlyArchivedRecordings(ctx, cutoff, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	items := make([]*ArchivedItem, 0, len(recs))
	for _, r := range recs {
		if r.EgressId == "" || !helpers.IsSoftDeleted(r.DeletedAt) {
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

// The doc restore's reads and writes: production wiring, which tests replace.
var (
	docsDeletion    = docDomain.GetDocsDeletion
	restoreDocNodes = docDomain.BulkRestoreDocs
	turnOffDocLinks = func(ctx context.Context, docUUID string) (int64, error) {
		return guestModel.RevokeLiveForResource(ctx, guestModel.ResourceDoc, docUUID)
	}
	tellOwnerLinksOff = pushLinksOffNotice
	// ownerReads is the doc read rule, of many people at once.
	ownerReads = docBusiness.Readers
)

// restoreDocs brings deleted docs back, in the graph and then in search, and
// answers how many it restored (a live doc, or with archivedOnly one its owner
// deleted, is left as it is and not counted).
//
// A doc its owner deleted comes back with its guest links off: the owner took
// it away from everyone, and its links, dead while it was deleted, opened it
// again to people outside the workspace without the owner's say. They are
// turned off before the restore, so a doc whose links can't be turned off
// stays deleted (and the error is answered), and the owner is told. A doc the
// retention archived keeps its links, whether a run is undone (archivedOnly)
// or an admin restores it from the list.
func restoreDocs(ctx context.Context, entityIds []uuid.UUID, archivedOnly bool) (int, error) {
	docUuids := make([]string, len(entityIds))
	for i, id := range entityIds {
		docUuids[i] = id.String()
	}
	ownerDeleted := map[string]*dgraphStruct.DgraphDoc{}
	linksOff := map[string]int64{}
	var linksErr error
	if !archivedOnly {
		docs, err := docsDeletion(ctx, docUuids)
		if err != nil {
			return 0, err
		}
		held := map[string]bool{}
		for _, d := range docs {
			if d == nil || d.Uuid == "" || !helpers.IsSoftDeleted(d.DeletedAt) || helpers.IsSoftDeleted(d.ArchivedAt) {
				continue // live, or archived by the retention
			}
			id := strings.ToLower(d.Uuid)
			n, err := turnOffDocLinks(ctx, id)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/restoreDocs could not turn off the guest links of doc %s, so it stays deleted: %v", id, err)
				linksErr = fmt.Errorf("a doc its owner deleted was not restored: its guest links could not be turned off: %w", err)
				held[id] = true
				continue
			}
			ownerDeleted[id], linksOff[id] = d, n
		}
		if len(held) > 0 {
			kept := make([]string, 0, len(docUuids))
			for _, id := range docUuids {
				if !held[strings.ToLower(id)] {
					kept = append(kept, id)
				}
			}
			docUuids = kept
		}
	}
	restored, err := restoreDocNodes(ctx, docUuids, archivedOnly)
	if len(restored) > 0 {
		docSearchRestore(restored)
		byID, byName := askingUser(ctx)
		for _, id := range restored {
			id = strings.ToLower(id)
			d, ok := ownerDeleted[id]
			if !ok || linksOff[id] == 0 {
				continue // no link went
			}
			if o := docDomain.OwnerOf(d); o == nil || o.Uuid == "" || o.Uuid == byID {
				continue // no one but who restored it to tell
			}
			tellOwnerLinksOff(ctx, d, linksOff[id], byName)
		}
	}
	if err == nil {
		err = linksErr
	}
	return len(restored), err
}

// pushLinksOffNotice tells a doc's owner (after a hand-over, the new one:
// docDomain.OwnerOf), on their devices, that someone restored the doc they
// deleted and turned its guest links off (linksOffNotice says what), when they
// may read it now (business/Doc Readers): its title goes to nobody else, and
// to nobody when that can't be said. Nothing told an owner about a restore;
// push is the notice this package can send (email, comments and the activity
// feed are in packages that depend on it). It opens the doc, as a push about a
// doc's comment does, and goes as every push does (notification.Deliver):
// through the push gate, with the desktop app's notice beside it, which a
// desktop-only owner had none of.
func pushLinksOffNotice(ctx context.Context, doc *dgraphStruct.DgraphDoc, links int64, byName string) {
	if doc == nil {
		return
	}
	owner := docDomain.OwnerOf(doc)
	if owner == nil || owner.Uuid == "" {
		return
	}
	can, err := ownerReads(ctx, doc.Uuid, []string{owner.Uuid})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/pushLinksOffNotice could not decide whether the owner of doc %s may read it, so nobody is told: %v", doc.Uuid, err)
		return
	}
	if !can[owner.Uuid] {
		return
	}
	title, body := linksOffNotice(byName, doc.Title, links)
	notificationBusiness.Deliver(ctx, []string{owner.Uuid}, map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:    firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: doc.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:   title,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:    body,
	}, "doc_restored:"+doc.Uuid+":"+strconv.FormatInt(time.Now().UnixNano(), 10))
}

// linksOffNotice is what a doc's owner is told when someone restores the doc
// they deleted and its guest links are turned off: who, which doc, and that
// guests get in again only when the owner shares it again. Pure.
func linksOffNotice(byName, title string, links int64) (heading, body string) {
	who := strings.TrimSpace(byName)
	if who == "" {
		who = "An admin"
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Untitled document"
	}
	off := "Its guest link was turned off"
	if links != 1 {
		off = fmt.Sprintf("Its %d guest links were turned off", links)
	}
	return "Your doc was restored", fmt.Sprintf("%s restored “%s”, which you deleted. %s; you can turn guest access back on by sharing it again.", who, title, off)
}

// docSearchRestore is the docs registry entry's search restore, named apart
// so restoreDocs need not read the registry while it is being built.
var docSearchRestore = docSearchFollows(makeOpenSearchRestore(docCascadeFields, docCascadeIndexes))

// StartAutoArchiver runs a background scheduler that executes archive jobs
// based on policies where auto_archive is true. Runs every hour after a 5-minute initial delay.
// The provided context is used for graceful shutdown.
func StartAutoArchiver(ctx context.Context) {
	helpers.GoSafeLoop("business/Archive/archiveBusiness:StartAutoArchiver", func() {
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
	})
}
