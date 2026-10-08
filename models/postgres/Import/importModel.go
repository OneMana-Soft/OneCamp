// Package models is the provider-agnostic postgres layer for the import
// pipeline. It owns every SQL touch on import_jobs, import_chunks,
// import_id_map, import_workspace_id_map, import_errors, plus the new
// per-import status/priority mapping tables and OAuth token storage.
//
// The Slack-specific package (models/postgres/SlackImport) is a thin
// shim that calls into here for new code paths and keeps the legacy
// column-name shape for the existing Slack worker code.
//
// Concurrency model:
//   - All worker mutations on import_chunks use SELECT ... FOR UPDATE
//     SKIP LOCKED so multiple goroutines/processes never claim the same
//     chunk.
//   - Inserts into import_id_map are ON CONFLICT DO NOTHING so a worker
//     retry after a partial-success crash is a no-op.
//   - Status transitions on import_jobs are guarded by the partial
//     unique index uq_import_jobs_active_per_provider_workspace; two
//     parallel "validating" runs for the same (provider, workspace)
//     surface as ErrConflictActiveJob.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Provider names — keep in sync with migration 60 CHECK constraints.
const (
	ProviderSlack   = "slack"
	ProviderAsana   = "asana"
	ProviderJira    = "jira"
	ProviderTrello  = "trello"
	ProviderNotion  = "notion"
	ProviderTodoist = "todoist"
	ProviderLinear  = "linear"
	ProviderClickUp = "clickup"
)

// Job statuses. Aligned with the CHECK constraint in migration 60.
const (
	StatusPending    = "pending"
	StatusValidating = "validating"
	StatusPlanned    = "planned"
	StatusRunning    = "running"
	StatusPaused     = "paused"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusRolledBack = "rolled_back"
)

// Source types. ZIPs are pre-uploaded to MinIO; api uses live tokens.
const (
	SourceExportZip    = "export_zip"
	SourceCorporateZip = "corporate_zip"
	SourceAPI          = "api"
	SourceBoardJSON    = "board_json"
	SourceBackupXML    = "backup_xml"
	SourceBackupCSV    = "backup_csv"
)

// Chunk types. Slack legacy plus task-provider stages.
const (
	// Slack-only.
	ChunkChannelMessages = "channel_messages"
	ChunkChannelThreads  = "channel_threads"
	ChunkDMMessages      = "dm_messages"
	ChunkFile            = "file"
	ChunkReactionPass    = "reaction_pass"
	// Task-provider stages.
	ChunkProjectTasks = "project_tasks"
	ChunkTaskSubtasks = "task_subtasks"
	ChunkTaskComments = "task_comments"
	ChunkAttachment   = "attachment"
)

// Chunk statuses.
const (
	ChunkStatusPending    = "pending"
	ChunkStatusInProgress = "in_progress"
	ChunkStatusDone       = "done"
	ChunkStatusFailed     = "failed"
	ChunkStatusSkipped    = "skipped"
	ChunkStatusCancelled  = "cancelled"
)

// Entity types in import_id_map. Slack legacy + task-management entities.
const (
	EntityUser    = "user"
	EntityChannel = "channel" // Slack
	EntityDM      = "dm"      // Slack
	EntityMPIM    = "mpim"    // Slack
	EntityMessage = "message" // Slack post/chat
	EntityComment = "comment" // Slack thread reply OR task comment
	EntityFile    = "file"    // attachment
	EntityTeam    = "team"
	EntityProject = "project"
	EntityTask    = "task"
	EntitySubtask = "subtask"
	EntityLabel   = "label"
	EntityField   = "task_field" // a project's custom field (business/Import/fields.go)
)

// Severity levels for error rows.
const (
	SeverityWarning = "warning"
	SeverityError   = "error"
	SeverityFatal   = "fatal"
)

// ErrJobNotFound is returned when the requested job id does not exist.
var ErrJobNotFound = errors.New("import job not found")

// ErrConflictActiveJob fires when a status transition would violate the
// active-per-(provider, workspace) partial unique index. Controllers
// surface this as 409 Conflict.
var ErrConflictActiveJob = errors.New("import already active for this provider/workspace")

// Job is the row shape of import_jobs.
type Job struct {
	Id                  uuid.UUID
	Provider            string
	SourceWorkspaceName string
	Source              string
	RawObjectKey        *string
	Status              string
	Stage               *string
	StartedAt           *time.Time
	CompletedAt         *time.Time
	Options             json.RawMessage
	Plan                json.RawMessage
	Progress            json.RawMessage
	ContentHash         *string
	ErrorMessage        *string
	TriggeredBy         *uuid.UUID
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Chunk is the row shape of import_chunks.
type Chunk struct {
	Id        uuid.UUID
	ImportId  uuid.UUID
	ChunkType string
	// Holds the source-side parent id (channel_slack_id legacy column;
	// for task providers we reuse it for project_source_id or
	// task_source_id depending on chunk_type).
	ParentSourceId *string
	ObjectKey      *string
	Status         string
	Attempts       int
	MaxAttempts    int
	ItemsTotal     *int
	ItemsDone      int
	LastCursor     *string
	ClaimedBy      *string
	ClaimedAt      *time.Time
	Error          *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// IdMapEntry is the (sourceId, onecampUUID) pair surfaced by lookup
// helpers. Used during rollback walks.
type IdMapEntry struct {
	SourceId    string
	OnecampUUID uuid.UUID
}

// ─── Job CRUD ─────────────────────────────────────────────────────────

// CreateJob inserts a new job row. ErrConflictActiveJob fires when the
// partial unique index trips (active import already exists for this
// provider+workspace).
func CreateJob(ctx context.Context, j *Job) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if j.Provider == "" {
		j.Provider = ProviderSlack
	}
	if len(j.Options) == 0 {
		j.Options = json.RawMessage(`{}`)
	}
	if len(j.Progress) == 0 {
		j.Progress = json.RawMessage(`{}`)
	}

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_jobs
		    (id, provider, source_workspace_name, source, raw_object_key,
		     status, options, progress, triggered_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, NOW(), NOW())
	`,
		j.Id, j.Provider, j.SourceWorkspaceName, j.Source, j.RawObjectKey,
		j.Status, j.Options, j.Progress, j.TriggeredBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflictActiveJob
		}
		helpers.LogErrorWithContext(ctx, "models/Import.CreateJob err: %+v", err)
		return err
	}
	return nil
}

// JobSelectColumns is the scan-coupled projection for a full import_jobs row.
// Exported so the domain layer can compose the same column order when building
// the (dynamic) job-list query, keeping a single source of truth for what
// scanJob expects.
const JobSelectColumns = `id, provider, source_workspace_name, source, raw_object_key,
	status, stage, started_at, completed_at, options, plan, progress,
	content_hash, error_message, triggered_by, created_at, updated_at`

func scanJob(row interface {
	Scan(dest ...interface{}) error
}) (*Job, error) {
	j := &Job{}
	err := row.Scan(
		&j.Id, &j.Provider, &j.SourceWorkspaceName, &j.Source, &j.RawObjectKey,
		&j.Status, &j.Stage, &j.StartedAt, &j.CompletedAt, &j.Options, &j.Plan, &j.Progress,
		&j.ContentHash, &j.ErrorMessage, &j.TriggeredBy, &j.CreatedAt, &j.UpdatedAt,
	)
	return j, err
}

// GetJob fetches a single job row by id.
func GetJob(ctx context.Context, jobId uuid.UUID) (*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT `+JobSelectColumns+` FROM import_jobs WHERE id = $1`, jobId)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// ExecJobs runs a pre-built import-job list query (see domain/Import, which
// owns the optional provider filter + ordering/limit) and scans the rows.
func ExecJobs(ctx context.Context, query string, args []interface{}) ([]*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Job, 0, 16)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateStatus transitions a job's status (and optionally stage). Sets
// started_at and completed_at where appropriate. ErrConflictActiveJob
// surfaces if the partial unique index trips during a transition.
//
// Safety: when transitioning TO 'running', refuse if the job is
// already in a terminal state (cancelled/failed/rolled_back/completed).
// This guards against a stage worker writing 'running' on top of an
// operator-issued cancel during a per-stage transition.
func UpdateStatus(ctx context.Context, jobId uuid.UUID, status string, stage *string, errMsg *string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs
		SET status = $2,
		    stage = COALESCE($3, stage),
		    error_message = $4,
		    started_at = CASE
		        WHEN $2 = 'running' AND started_at IS NULL THEN NOW()
		        ELSE started_at END,
		    completed_at = CASE
		        WHEN $2 IN ('completed','failed','cancelled','rolled_back') THEN NOW()
		        ELSE completed_at END,
		    updated_at = NOW()
		WHERE id = $1
		  AND NOT (
		      $2 = 'running' AND status IN ('cancelled','failed','rolled_back','completed')
		  )`, jobId, status, stage, errMsg)
	if err != nil && isUniqueViolation(err) {
		return ErrConflictActiveJob
	}
	return err
}

// UpdatePlan stores the plan and atomically advances status to planned.
func UpdatePlan(ctx context.Context, jobId uuid.UUID, plan json.RawMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs SET plan = $2, status = 'planned', stage = 'planned', updated_at = NOW()
		WHERE id = $1`, jobId, plan)
	return err
}

// SetStageIfRunning updates stage only when the job is still in the
// running state. Returns true on update, false when the job has been
// cancelled / failed / rolled-back / completed in the meantime.
//
// This closes a race between CancelImport (which writes status='cancelled')
// and the orchestrator's per-stage setStage call. The previous code
// path used UpdateStatus(running, stage) which would clobber the
// operator's cancellation if the orchestrator was between stages.
func SetStageIfRunning(ctx context.Context, jobId uuid.UUID, stage string) (bool, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs
		SET stage = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'running'`, jobId, stage)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateOptions overwrites the options JSON. Used when the operator
// tweaks knobs after planning.
func UpdateOptions(ctx context.Context, jobId uuid.UUID, options json.RawMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE import_jobs SET options = $2, updated_at = NOW() WHERE id = $1`, jobId, options)
	return err
}

// UpdateProgress shallow-merges the patch into the progress JSONB column.
// Workers can call this concurrently because PG's `||` jsonb merge is atomic.
func UpdateProgress(ctx context.Context, jobId uuid.UUID, patch json.RawMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs
		SET progress = COALESCE(progress, '{}'::jsonb) || $2::jsonb,
		    updated_at = NOW()
		WHERE id = $1`, jobId, patch)
	return err
}

// ListJobsForRawCleanup returns terminal jobs older than cutoff that
// still have a raw_object_key set. Bounded at 200 rows per call.
func ListJobsForRawCleanup(ctx context.Context, cutoff time.Time) ([]*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT `+JobSelectColumns+` FROM import_jobs
		WHERE status IN ('completed','failed','cancelled','rolled_back')
		  AND raw_object_key IS NOT NULL
		  AND COALESCE(completed_at, updated_at) < $1
		ORDER BY COALESCE(completed_at, updated_at)
		LIMIT 200`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Job, 0, 16)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListAbandonedPendingJobs returns 'pending' jobs older than cutoff.
// These are jobs whose presigned upload was never finalised.
func ListAbandonedPendingJobs(ctx context.Context, cutoff time.Time) ([]*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT `+JobSelectColumns+` FROM import_jobs
		WHERE status = 'pending'
		  AND created_at < $1
		ORDER BY created_at
		LIMIT 200`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Job, 0, 16)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClearRawObjectKey nullifies raw_object_key after MinIO deletion.
func ClearRawObjectKey(ctx context.Context, jobId uuid.UUID) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE import_jobs SET raw_object_key = NULL, updated_at = NOW() WHERE id = $1`, jobId)
	return err
}

// CountChunks returns (total, done, failed) for FE summary.
func CountChunks(ctx context.Context, jobId uuid.UUID) (int, int, int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT
		    COUNT(*) AS total,
		    COUNT(*) FILTER (WHERE status = 'done') AS done,
		    COUNT(*) FILTER (WHERE status = 'failed') AS failed
		FROM import_chunks WHERE import_id = $1`, jobId)
	var total, done, failed int
	err := row.Scan(&total, &done, &failed)
	return total, done, failed, err
}

// CountErrors counts error rows for the FE badge.
func CountErrors(ctx context.Context, jobId uuid.UUID) (int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT COUNT(*) FROM import_errors WHERE import_id = $1`, jobId)
	var n int
	return n, row.Scan(&n)
}

// SumItemsImported returns the total items_done across all chunks.
func SumItemsImported(ctx context.Context, jobId uuid.UUID) (int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT COALESCE(SUM(items_done),0) FROM import_chunks WHERE import_id = $1`, jobId)
	var n int
	return n, row.Scan(&n)
}

// ─── Chunks ───────────────────────────────────────────────────────────

// CreateChunks bulk-inserts chunks, idempotent on the logical key.
// Batches at 500 to keep bind-param count well under PG's ~32k limit.
func CreateChunks(ctx context.Context, chunks []*Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	const batchSize = 500
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]
		args := make([]interface{}, 0, len(batch)*8)
		values := make([]string, 0, len(batch))
		for i, c := range batch {
			off := i * 8
			values = append(values, fmt.Sprintf(
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				off+1, off+2, off+3, off+4, off+5, off+6, off+7, off+8,
			))
			args = append(args,
				c.Id, c.ImportId, c.ChunkType, c.ParentSourceId, c.ObjectKey,
				c.Status, c.MaxAttempts, c.ItemsTotal,
			)
		}
		// channel_slack_id is the legacy SQL column name; we keep it to
		// preserve the existing Slack workers, and reuse it for the
		// task providers as parent_source_id at the application layer.
		query := fmt.Sprintf(`
			INSERT INTO import_chunks
			    (id, import_id, chunk_type, channel_slack_id, object_key,
			     status, max_attempts, items_total)
			VALUES %s
			ON CONFLICT (import_id, chunk_type,
			             COALESCE(channel_slack_id, ''),
			             COALESCE(object_key, '')) DO NOTHING
		`, strings.Join(values, ","))

		if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, query, args...); err != nil {
			return fmt.Errorf("CreateChunks batch %d: %w", start, err)
		}
	}
	return nil
}

const chunkSelectColumns = `id, import_id, chunk_type, channel_slack_id, object_key,
	status, attempts, max_attempts, items_total, items_done,
	last_cursor, claimed_by, claimed_at, error, created_at, updated_at`

func scanChunk(row interface {
	Scan(dest ...interface{}) error
}) (*Chunk, error) {
	c := &Chunk{}
	err := row.Scan(
		&c.Id, &c.ImportId, &c.ChunkType, &c.ParentSourceId, &c.ObjectKey,
		&c.Status, &c.Attempts, &c.MaxAttempts, &c.ItemsTotal, &c.ItemsDone,
		&c.LastCursor, &c.ClaimedBy, &c.ClaimedAt, &c.Error, &c.CreatedAt, &c.UpdatedAt,
	)
	return c, err
}

// ClaimNextChunk atomically picks one runnable chunk for the worker.
// Pattern: SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1, then UPDATE.
// Workers may filter by chunkTypes (empty == any).
func ClaimNextChunk(ctx context.Context, importId uuid.UUID, chunkTypes []string, workerId string) (*Chunk, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbCtx, nil)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	typeClause := ""
	args := []interface{}{importId}
	if len(chunkTypes) > 0 {
		placeholders := make([]string, 0, len(chunkTypes))
		for _, t := range chunkTypes {
			args = append(args, t)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		typeClause = " AND chunk_type IN (" + strings.Join(placeholders, ",") + ")"
	}

	row := tx.QueryRowContext(dbCtx, fmt.Sprintf(`
		SELECT %s FROM import_chunks
		WHERE import_id = $1
		  AND status IN ('pending','failed')
		  AND attempts < max_attempts
		  %s
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, chunkSelectColumns, typeClause), args...)

	c, err := scanChunk(row)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Commit()
		committed = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status = 'in_progress',
		    attempts = attempts + 1,
		    claimed_by = $2,
		    claimed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1`, c.Id, workerId); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	c.Status = ChunkStatusInProgress
	c.Attempts++
	c.ClaimedBy = &workerId
	return c, nil
}

// FinishChunk marks a chunk done with final counts.
func FinishChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='done', items_done=$2, last_cursor=COALESCE($3, last_cursor),
		    error=NULL, updated_at=NOW()
		WHERE id=$1`, chunkId, itemsDone, lastCursor)
	return err
}

// FailChunk marks a chunk failed with an error message; eligible for
// retry while attempts < max_attempts.
func FailChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string, errMsg string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='failed', items_done=$2, last_cursor=COALESCE($3, last_cursor),
		    error=$4, updated_at=NOW()
		WHERE id=$1`, chunkId, itemsDone, lastCursor, errMsg)
	return err
}

// HeartbeatChunk extends the claim while a long-running chunk processes.
func HeartbeatChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET items_done=$2, last_cursor=COALESCE($3, last_cursor),
		    claimed_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND status='in_progress'`, chunkId, itemsDone, lastCursor)
	return err
}

// ResetChunkForRetry sends a chunk back to 'pending' WITHOUT bumping
// attempts. Used for transient conditions (rate-limit, retry-after).
func ResetChunkForRetry(ctx context.Context, chunkId uuid.UUID, reason string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='pending',
		    attempts=GREATEST(attempts-1, 0),
		    claimed_by=NULL, claimed_at=NULL,
		    error=$2, updated_at=NOW()
		WHERE id=$1`, chunkId, reason)
	return err
}

// ReapStuckChunks resets in_progress chunks whose claim is older than
// staleAfter back to pending so another worker can pick them up.
func ReapStuckChunks(ctx context.Context, staleAfter time.Duration) (int64, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='pending', claimed_by=NULL, claimed_at=NULL, updated_at=NOW()
		WHERE status='in_progress'
		  AND claimed_at < NOW() - $1::interval`,
		fmt.Sprintf("%d milliseconds", staleAfter.Milliseconds()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CancelAllPendingChunks bulk-cancels every non-terminal chunk for a job.
func CancelAllPendingChunks(ctx context.Context, importId uuid.UUID) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='cancelled', updated_at=NOW()
		WHERE import_id=$1 AND status IN ('pending','failed','in_progress')`, importId)
	return err
}

// ExecCount runs a pre-built COUNT(*) query (see domain/Import, which owns the
// optional chunk-type filter) and returns the scalar count.
func ExecCount(ctx context.Context, query string, args []interface{}) (int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ─── ID maps ──────────────────────────────────────────────────────────

// UpsertIdMapping records the source→OneCamp mapping. Idempotent.
// Workers call this BEFORE the actual data write so a partial-success
// crash is replayable.
func UpsertIdMapping(ctx context.Context, importId uuid.UUID, entityType, sourceId string,
	onecampUUID uuid.UUID, parentSourceId *string, metadata json.RawMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_id_map
		    (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (import_id, entity_type, source_id) DO NOTHING`,
		importId, entityType, sourceId, onecampUUID, parentSourceId, metadata)
	return err
}

// UpsertIdMappingWithOwnership is UpsertIdMapping plus the
// created_by_this_import flag for rollback ownership tracking.
func UpsertIdMappingWithOwnership(ctx context.Context, importId uuid.UUID,
	entityType, sourceId string, onecampUUID uuid.UUID, parentSourceId *string,
	metadata json.RawMessage, createdByThisImport bool) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_id_map
		    (import_id, entity_type, source_id, onecamp_uuid, parent_source_id,
		     metadata, created_by_this_import)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (import_id, entity_type, source_id) DO NOTHING`,
		importId, entityType, sourceId, onecampUUID, parentSourceId,
		metadata, createdByThisImport)
	return err
}

// LookupIdMapping fetches the OneCamp UUID for a source id. Returns
// uuid.Nil and nil when not found.
func LookupIdMapping(ctx context.Context, importId uuid.UUID, entityType, sourceId string) (uuid.UUID, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT onecamp_uuid FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND source_id = $3`,
		importId, entityType, sourceId)
	var u uuid.UUID
	err := row.Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return u, err
}

// ExecSourceIdMap runs a pre-built (source_id, onecamp_uuid) lookup query (see
// domain/Import, which owns the dynamic source_id IN(...) placeholder set) and
// scans it into a source_id -> OneCamp uuid map. Shared by the per-import and
// cross-import batch resolvers. hint sizes the result map.
func ExecSourceIdMap(ctx context.Context, query string, args []interface{}, hint int) (map[string]uuid.UUID, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]uuid.UUID, hint)
	for rows.Next() {
		var s string
		var u uuid.UUID
		if err := rows.Scan(&s, &u); err != nil {
			return nil, err
		}
		out[s] = u
	}
	return out, rows.Err()
}

// IdMappingsByTypeOwned returns mapping rows where this import was the
// physical creator. Used by rollback so we never soft-delete entities
// that a different import owns.
func IdMappingsByTypeOwned(ctx context.Context, importId uuid.UUID, entityType string) ([]IdMapEntry, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT source_id, onecamp_uuid FROM import_id_map
		WHERE import_id = $1
		  AND entity_type = $2
		  AND created_by_this_import = true
		ORDER BY source_id`, importId, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IdMapEntry, 0, 64)
	for rows.Next() {
		var e IdMapEntry
		if err := rows.Scan(&e.SourceId, &e.OnecampUUID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SourceIdsFor is the source ids this import mapped to one OneCamp entity.
func SourceIdsFor(ctx context.Context, importId uuid.UUID, entityType string, onecampUUID uuid.UUID) ([]string, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT source_id FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND onecamp_uuid = $3`,
		importId, entityType, onecampUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetIdMapMetadata returns the JSONB metadata blob for one mapping row.
// Returns nil bytes + nil error when missing.
func GetIdMapMetadata(ctx context.Context, importId uuid.UUID, entityType, sourceId string) (json.RawMessage, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT metadata FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND source_id = $3`,
		importId, entityType, sourceId)
	var raw []byte
	err := row.Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// ─── Workspace map (cross-import dedup) ───────────────────────────────

// LookupWorkspaceMapping returns the OneCamp UUID a previous import of
// this (provider, workspace) created for the given (entity, sourceId).
// Returns uuid.Nil and nil when not found.
func LookupWorkspaceMapping(ctx context.Context, provider, workspace, entityType, sourceId string) (uuid.UUID, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT onecamp_uuid FROM import_workspace_id_map
		WHERE provider = $1 AND source_workspace_name = $2
		  AND entity_type = $3 AND source_id = $4`,
		provider, workspace, entityType, sourceId)
	var u uuid.UUID
	err := row.Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return u, err
}

// LookupWorkspaceMappingsBatch (cross-import batch resolver) now lives in
// domain/Import, which builds the source_id IN(...) set and calls
// ExecSourceIdMap. Removed from the model to keep query assembly in the domain
// layer.

// UpsertWorkspaceMapping persists the cross-import mapping. Idempotent.
func UpsertWorkspaceMapping(ctx context.Context, provider, workspace, entityType, sourceId string,
	onecampUUID uuid.UUID, originImportId uuid.UUID) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_workspace_id_map
		    (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider, source_workspace_name, entity_type, source_id) DO NOTHING`,
		provider, workspace, entityType, sourceId, onecampUUID, originImportId)
	return err
}

// ─── Errors ───────────────────────────────────────────────────────────

// LogImportError persists a single error/warning/fatal entry.
// Best-effort; never blocks a worker on telemetry.
func LogImportError(ctx context.Context, importId uuid.UUID, chunkId *uuid.UUID,
	entityType, sourceId, severity, code, message string, errorContext json.RawMessage) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return // not connected (a provider's unit test): nowhere to keep it
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_errors
		    (import_id, chunk_id, entity_type, source_id, severity, code, message, context)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, NULLIF($6,''), $7, $8)`,
		importId, chunkId, entityType, sourceId, severity, code, message, errorContext)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Import.LogImportError swallowed err: %+v", err)
	}
}

// ListErrors returns up to limit error rows, optionally filtered by severity.
// Each row carries the source id as "source_id", and again as "slack_id" for
// the legacy Slack screen.
func ListErrors(ctx context.Context, importId uuid.UUID, severity string, limit, offset int) ([]map[string]interface{}, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	args := []interface{}{importId, limit, offset}
	sevClause := ""
	if severity != "" {
		args = append(args, severity)
		sevClause = " AND severity = $4"
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, entity_type, source_id, severity, code, message, context, created_at
		FROM import_errors
		WHERE import_id = $1`+sevClause+`
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]interface{}, 0, 32)
	for rows.Next() {
		var (
			id         uuid.UUID
			entityType *string
			sourceId   *string
			sev        string
			code       *string
			msg        string
			contextRaw []byte
			createdAt  time.Time
		)
		if err := rows.Scan(&id, &entityType, &sourceId, &sev, &code, &msg, &contextRaw, &createdAt); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id":          id,
			"entity_type": entityType,
			"source_id":   sourceId,
			"slack_id":    sourceId, // backward-compat for the legacy FE field
			"severity":    sev,
			"code":        code,
			"message":     msg,
			"created_at":  createdAt,
		}
		if len(contextRaw) > 0 {
			row["context"] = json.RawMessage(contextRaw)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ─── Status / priority mapping ────────────────────────────────────────

// SetStatusMappings replaces the per-import status map in one tx.
// Operator confirms in the Plan UI; the task worker reads this once.
func SetStatusMappings(ctx context.Context, importId uuid.UUID, mapping map[string]string) error {
	if mapping == nil {
		mapping = map[string]string{}
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbCtx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(dbCtx,
		`DELETE FROM import_status_mappings WHERE import_id = $1`, importId); err != nil {
		return err
	}
	for src, tgt := range mapping {
		src = strings.TrimSpace(strings.ToLower(src))
		if src == "" {
			continue
		}
		if _, err := tx.ExecContext(dbCtx, `
			INSERT INTO import_status_mappings (import_id, source_status, target_status)
			VALUES ($1, $2, $3)`, importId, src, tgt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// GetStatusMappings returns the operator-confirmed source→target map.
func GetStatusMappings(ctx context.Context, importId uuid.UUID) (map[string]string, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx,
		`SELECT source_status, target_status FROM import_status_mappings WHERE import_id = $1`, importId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, 8)
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			return nil, err
		}
		out[s] = t
	}
	return out, rows.Err()
}

// SetPriorityMappings is the priority equivalent of SetStatusMappings.
func SetPriorityMappings(ctx context.Context, importId uuid.UUID, mapping map[string]string) error {
	if mapping == nil {
		mapping = map[string]string{}
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbCtx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(dbCtx,
		`DELETE FROM import_priority_mappings WHERE import_id = $1`, importId); err != nil {
		return err
	}
	for src, tgt := range mapping {
		src = strings.TrimSpace(strings.ToLower(src))
		if src == "" {
			continue
		}
		if _, err := tx.ExecContext(dbCtx, `
			INSERT INTO import_priority_mappings (import_id, source_priority, target_priority)
			VALUES ($1, $2, $3)`, importId, src, tgt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// GetPriorityMappings returns the operator-confirmed priority map.
func GetPriorityMappings(ctx context.Context, importId uuid.UUID) (map[string]string, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx,
		`SELECT source_priority, target_priority FROM import_priority_mappings WHERE import_id = $1`, importId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, 4)
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			return nil, err
		}
		out[s] = t
	}
	return out, rows.Err()
}

// ─── Bridge helpers (used by rollback / direct SQL paths) ─────────────

// Exec is a passthrough for raw queries used by the import package
// itself (e.g., backdating posts after creation).
func Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return postgresInit.DBConn.SqlDB.ExecContext(ctx, query, args...)
}

// ExecQueryRow exposes QueryRowContext via this package, mirroring Exec.
// Note the returned row is bound to the package-level connection pool's
// context; per-call timeouts must be set by the caller.
func ExecQueryRow(ctx context.Context, query string, args ...interface{}) (*sql.Row, error) {
	return postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, args...), nil
}

// UpsertIdMappingThenFinish atomically writes the id_map row and marks
// the chunk done. Used by single-item chunks (e.g. file workers).
func UpsertIdMappingThenFinish(ctx context.Context, importId, chunkId uuid.UUID,
	entityType, sourceId string, onecampUUID uuid.UUID, metadata json.RawMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbCtx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(dbCtx, `
		INSERT INTO import_id_map
		    (import_id, entity_type, source_id, onecamp_uuid, metadata)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (import_id, entity_type, source_id) DO NOTHING`,
		importId, entityType, sourceId, onecampUUID, metadata); err != nil {
		return err
	}
	if _, err := tx.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='done', items_done=1, error=NULL, updated_at=NOW()
		WHERE id=$1`, chunkId); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────

// isUniqueViolation detects PG error code 23505 from any driver without
// importing pgx- or lib/pq-specific types here. Checks the error text
// because both drivers surface "23505" or "duplicate key value" in the
// message.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "duplicate key value") || strings.Contains(s, "23505")
}

// RetryFailedChunks resets every chunk in this job that's in 'failed'
// status with attempts >= max_attempts back to 'pending' with attempts=0.
// Used by an admin "retry stuck chunks" button when a transient
// upstream blip burned a chunk's retry budget.
//
// Returns the number of chunks reset. Idempotent — calling it twice
// on a job with no failed chunks is a no-op.
func RetryFailedChunks(ctx context.Context, importId uuid.UUID) (int64, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status='pending',
		    attempts=0,
		    claimed_by=NULL, claimed_at=NULL,
		    error=NULL,
		    updated_at=NOW()
		WHERE import_id=$1 AND status='failed'`, importId)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
