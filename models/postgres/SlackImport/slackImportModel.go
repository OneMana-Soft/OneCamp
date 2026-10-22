// Package models holds the postgres-side types and SQL helpers for the
// Slack import pipeline. Code in here MUST stay free of business logic;
// business decisions (concurrency, ordering, etc.) live one layer up.
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
	"github.com/akashc777/OneCamp/models/pgexec"
	jobs "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// Job statuses. Keep aligned with the CHECK constraint in migration 60.
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

// Sources.
const (
	SourceExportZip    = "export_zip"
	SourceCorporateZip = "corporate_zip"
	SourceAPI          = "api"
)

// Chunk types.
const (
	ChunkChannelMessages = "channel_messages"
	ChunkChannelThreads  = "channel_threads"
	ChunkDMMessages      = "dm_messages"
	ChunkFile            = "file"
	ChunkReactionPass    = "reaction_pass"
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

// Entity types in import_id_map.
const (
	EntityUser    = "user"
	EntityChannel = "channel"
	EntityDM      = "dm"
	EntityMPIM    = "mpim"
	EntityMessage = "message"
	EntityComment = "comment"
	EntityFile    = "file"
)

// Severity levels for errors.
const (
	SeverityWarning = "warning"
	SeverityError   = "error"
	SeverityFatal   = "fatal"
)

// ErrJobNotFound is returned when the requested job id does not exist.
var ErrJobNotFound = errors.New("slack import job not found")

// ErrConflictActiveJob is returned when an attempt to create a new job
// for a workspace would violate the active-per-workspace partial unique
// index. Callers should surface this as 409 Conflict. It is the generic
// import's, since status changes go through that package (UpdateStatus).
var ErrConflictActiveJob = jobs.ErrConflictActiveJob

// ErrJobChanged is returned when a job is no longer in a status a change was
// made for: another request moved it in the meantime. Callers should surface
// this as 409 Conflict.
var ErrJobChanged = jobs.ErrJobChanged

// Job is the row shape of import_jobs.
//
// Plan is NULL until the job is planned and is scanned through a *[]byte,
// which is the only destination database/sql stores a NULL into: scanned as
// the json.RawMessage it is, every uploaded export waiting to be planned failed
// to read (see models/postgres/Import scanJob).
type Job struct {
	Id                 uuid.UUID
	SlackWorkspaceName string
	Source             string
	RawObjectKey       *string
	Status             string
	Stage              *string
	StartedAt          *time.Time
	CompletedAt        *time.Time
	Options            json.RawMessage
	Plan               json.RawMessage
	Progress           json.RawMessage
	ErrorMessage       *string
	// Digest is the AI's prose account of what this import brought in. Nil on
	// the AI-free edition, with AI switched off, and on every job that ran
	// before the column existed.
	Digest      *string
	TriggeredBy *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Chunk is the row shape of import_chunks.
type Chunk struct {
	Id             uuid.UUID
	ImportId       uuid.UUID
	ChunkType      string
	ChannelSlackId *string
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

// CreateJob inserts a new job row. Returns ErrConflictActiveJob if the
// partial unique index trips.
func CreateJob(ctx context.Context, j *Job) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if len(j.Options) == 0 {
		j.Options = json.RawMessage(`{}`)
	}
	if len(j.Progress) == 0 {
		j.Progress = json.RawMessage(`{}`)
	}

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_jobs
			(id, provider, source_workspace_name, source, raw_object_key, status, options, progress, triggered_by, created_at, updated_at)
		VALUES ($1, 'slack', $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
	`,
		j.Id,
		j.SlackWorkspaceName,
		j.Source,
		j.RawObjectKey,
		j.Status,
		j.Options,
		j.Progress,
		j.TriggeredBy,
	)
	if err != nil {
		// Postgres returns 23505 for unique_violation; the partial unique
		// index uq_import_jobs_active_per_workspace surfaces here.
		if isUniqueViolation(err) {
			return ErrConflictActiveJob
		}
		helpers.LogErrorWithContext(ctx,
			"models/SlackImport.CreateJob failed err: %+v", err)
		return err
	}
	return nil
}

// GetJob fetches a single job row by id.
func GetJob(ctx context.Context, jobId uuid.UUID) (*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT id, source_workspace_name, source, raw_object_key, status, stage,
		       started_at, completed_at, options, plan, progress,
		       error_message, digest, triggered_by, created_at, updated_at
		FROM import_jobs WHERE id = $1
	`, jobId)

	j := &Job{}
	err := row.Scan(
		&j.Id, &j.SlackWorkspaceName, &j.Source, &j.RawObjectKey, &j.Status, &j.Stage,
		&j.StartedAt, &j.CompletedAt, &j.Options, (*[]byte)(&j.Plan), &j.Progress,
		&j.ErrorMessage, &j.Digest, &j.TriggeredBy, &j.CreatedAt, &j.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/SlackImport.GetJob failed err: %+v", err)
		return nil, err
	}
	return j, nil
}

// ListJobs returns up to limit jobs ordered by created_at desc.
func ListJobs(ctx context.Context, limit int) ([]*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, source_workspace_name, source, raw_object_key, status, stage,
		       started_at, completed_at, options, plan, progress,
		       error_message, digest, triggered_by, created_at, updated_at
		FROM import_jobs
		WHERE provider = 'slack'
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Job, 0, 16)
	for rows.Next() {
		j := &Job{}
		if err := rows.Scan(
			&j.Id, &j.SlackWorkspaceName, &j.Source, &j.RawObjectKey, &j.Status, &j.Stage,
			&j.StartedAt, &j.CompletedAt, &j.Options, (*[]byte)(&j.Plan), &j.Progress,
			&j.ErrorMessage, &j.Digest, &j.TriggeredBy, &j.CreatedAt, &j.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateStatus transitions a job's status (and optionally stage). Used at
// every lifecycle boundary. It is the generic import's (models/postgres/Import),
// which writes the same table: Slack's own copy had no guard, so a stage
// writing "running" after an operator's cancel ran the import on.
func UpdateStatus(ctx context.Context, jobId uuid.UUID, status string, stage *string, errMsg *string) error {
	return jobs.UpdateStatus(ctx, jobId, status, stage, errMsg)
}

// UpdateStatusFrom is UpdateStatus only from one of the statuses in from, in
// one statement, and reports whether it moved the job.
func UpdateStatusFrom(ctx context.Context, jobId uuid.UUID, from []string, status string, stage *string, errMsg *string) (bool, error) {
	return jobs.UpdateStatusFrom(ctx, jobId, from, status, stage, errMsg)
}

// RollBack moves a finished job to rolled_back and runs undo in the same
// transaction: all of it happens or none of it does (the generic import's).
func RollBack(ctx context.Context, jobId uuid.UUID, undo func(tx *sql.Tx) error) error {
	return jobs.RollBack(ctx, jobId, undo)
}

// SavePlan stores the planning result and the chunks the import runs in, and
// moves the job to planned, only while it waits to be planned; ErrJobChanged
// otherwise. It is the generic import's: Slack's own copy wrote "planned"
// over a job Run had started in the meantime, and Run started it again.
func SavePlan(ctx context.Context, jobId uuid.UUID, plan json.RawMessage, chunks []*Chunk) error {
	generic := make([]*jobs.Chunk, len(chunks))
	for i, c := range chunks {
		generic[i] = &jobs.Chunk{
			Id: c.Id, ImportId: c.ImportId, ChunkType: c.ChunkType, ParentSourceId: c.ChannelSlackId,
			ObjectKey: c.ObjectKey, Status: c.Status, Attempts: c.Attempts, MaxAttempts: c.MaxAttempts,
			ItemsTotal: c.ItemsTotal, ItemsDone: c.ItemsDone, LastCursor: c.LastCursor, ClaimedBy: c.ClaimedBy,
			ClaimedAt: c.ClaimedAt, Error: c.Error, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		}
	}
	return jobs.SavePlan(ctx, jobId, plan, generic)
}

// UpdateOptions is called by Run if the operator tweaked knobs after planning.
func UpdateOptions(ctx context.Context, jobId uuid.UUID, options json.RawMessage) error {
	return jobs.UpdateOptions(ctx, jobId, options)
}

// UpdateProgress merges a progress JSONB patch into the existing column.
// The patch is shallow-merged with jsonb || jsonb so workers can update
// individual stage counters without reading-modifying-writing the whole
// blob and racing each other.
func UpdateProgress(ctx context.Context, jobId uuid.UUID, patch json.RawMessage) error {
	return jobs.UpdateProgress(ctx, jobId, patch)
}

// UpdateDigest stores the prose account of what this import brought in.
//
// Write-once by convention: the digest is produced at finalize and there is no
// second producer. It is not part of UpdateStatus because it arrives strictly
// after the job is already completed, and delaying the completed status until a
// model call returns would make the import look slower than it was.
func UpdateDigest(ctx context.Context, jobId uuid.UUID, digest string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs
		SET digest = $2, updated_at = NOW()
		WHERE id = $1`, jobId, digest)
	return err
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

// CreateChunks bulk-inserts chunks for a job. Used during planning.
// Idempotent on (import_id, chunk_type, COALESCE(channel_slack_id,”),
// COALESCE(object_key,”)) to make replanning a no-op.
func CreateChunks(ctx context.Context, chunks []*Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	// We use a multi-row INSERT … ON CONFLICT DO NOTHING. Postgres tops
	// out around ~32k bind params; we keep batch to 500 chunks to stay
	// well under that.
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
				c.Id, c.ImportId, c.ChunkType, c.ChannelSlackId, c.ObjectKey,
				c.Status, c.MaxAttempts, c.ItemsTotal,
			)
		}

		query := fmt.Sprintf(`
			INSERT INTO import_chunks
			    (id, import_id, chunk_type, channel_slack_id, object_key,
			     status, max_attempts, items_total)
			VALUES %s
			ON CONFLICT (import_id, chunk_type,
			             COALESCE(channel_slack_id, ''),
			             COALESCE(object_key, '')) DO NOTHING
		`, joinComma(values))

		if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, query, args...); err != nil {
			return fmt.Errorf("CreateChunks batch %d: %w", start, err)
		}
	}
	return nil
}

// ClaimNextChunk atomically picks one runnable chunk for the worker.
// Pattern: SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1, then UPDATE.
// Workers may filter by chunkTypes (e.g. file workers only claim "file"
// chunks). Empty slice means "any type".
func ClaimNextChunk(ctx context.Context, importId uuid.UUID, chunkTypes []string, workerId string) (*Chunk, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbCtx, nil)
	if err != nil {
		return nil, err
	}
	// Always end the tx; the named-return makes commit/rollback explicit.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Build the type filter inline; using = ANY($X) is cleaner but the
	// driver handles []string only via pq.Array which isn't imported here,
	// so we do a simple OR. Worker count is small (≤16) so this is fine.
	typeClause := ""
	args := []interface{}{importId}
	if len(chunkTypes) > 0 {
		placeholders := make([]string, 0, len(chunkTypes))
		for _, t := range chunkTypes {
			args = append(args, t)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		typeClause = " AND chunk_type IN (" + joinComma(placeholders) + ")"
	}

	row := tx.QueryRowContext(dbCtx, fmt.Sprintf(`
		SELECT id, import_id, chunk_type, channel_slack_id, object_key,
		       status, attempts, max_attempts, items_total, items_done,
		       last_cursor, claimed_by, claimed_at, error, created_at, updated_at
		FROM import_chunks
		WHERE import_id = $1
		  AND status IN ('pending','failed')
		  AND attempts < max_attempts
		  %s
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, typeClause), args...)

	c := &Chunk{}
	err = row.Scan(
		&c.Id, &c.ImportId, &c.ChunkType, &c.ChannelSlackId, &c.ObjectKey,
		&c.Status, &c.Attempts, &c.MaxAttempts, &c.ItemsTotal, &c.ItemsDone,
		&c.LastCursor, &c.ClaimedBy, &c.ClaimedAt, &c.Error, &c.CreatedAt, &c.UpdatedAt,
	)
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
		WHERE id = $1
	`, c.Id, workerId); err != nil {
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

// FinishChunk marks the chunk as done with final counts.
func FinishChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string) error {
	return jobs.FinishChunk(ctx, chunkId, itemsDone, lastCursor)
}

// FailChunk marks the chunk as failed with an error message. It will be
// re-claimed if attempts < max_attempts.
func FailChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string, errMsg string) error {
	return jobs.FailChunk(ctx, chunkId, itemsDone, lastCursor, errMsg)
}

// HeartbeatChunk extends the claim while a long-running chunk is being
// processed. Called periodically by workers; the reaper uses claimed_at
// to detect stuck chunks so heartbeating prevents false-positive resets.
func HeartbeatChunk(ctx context.Context, chunkId uuid.UUID, itemsDone int, lastCursor *string) error {
	return jobs.HeartbeatChunk(ctx, chunkId, itemsDone, lastCursor)
}

// CancelAllPendingChunks bulk-cancels every non-terminal chunk for a job.
// Called when the operator clicks Cancel; in-progress workers detect the
// job-status change via their periodic check and exit gracefully.
func CancelAllPendingChunks(ctx context.Context, importId uuid.UUID) error {
	return jobs.CancelAllPendingChunks(ctx, importId)
}

// HasPendingWork returns true if any chunk for the job is still runnable.
// Used by the orchestrator to decide whether to advance to the next stage.
func HasPendingWork(ctx context.Context, importId uuid.UUID, chunkTypes []string) (bool, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	args := []interface{}{importId}
	clause := ""
	if len(chunkTypes) > 0 {
		placeholders := make([]string, 0, len(chunkTypes))
		for _, t := range chunkTypes {
			args = append(args, t)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		clause = " AND chunk_type IN (" + joinComma(placeholders) + ")"
	}

	var n int
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, fmt.Sprintf(`
		SELECT COUNT(*) FROM import_chunks
		WHERE import_id = $1
		  AND status IN ('pending','in_progress','failed')
		  AND attempts < max_attempts
		  %s`, clause), args...)
	if err := row.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- ID map ---------------------------------------------------------------

// UpsertIdMapping records the Slack→OneCamp mapping. Idempotent on the
// composite primary key. Workers call this before the actual data write
// so a crash mid-write is replayable.
func UpsertIdMapping(ctx context.Context, importId uuid.UUID, entityType, slackId string,
	onecampUUID uuid.UUID, parentSlackId *string, metadata json.RawMessage) error {
	return jobs.UpsertIdMapping(ctx, importId, entityType, slackId, onecampUUID, parentSlackId, metadata)
}

// LookupIdMapping fetches the OneCamp UUID for a Slack id. Returns
// uuid.Nil and nil error when not found, so callers can branch cleanly.
func LookupIdMapping(ctx context.Context, importId uuid.UUID, entityType, slackId string) (uuid.UUID, error) {
	return jobs.LookupIdMapping(ctx, importId, entityType, slackId)
}

// LookupIdMappingsBatch resolves N slack_ids in one round-trip. Saves
// dozens of queries during reaction & mention resolution.
func LookupIdMappingsBatch(ctx context.Context, importId uuid.UUID, entityType string, slackIds []string) (map[string]uuid.UUID, error) {
	if len(slackIds) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	args := make([]interface{}, 0, len(slackIds)+2)
	args = append(args, importId, entityType)
	placeholders := make([]string, 0, len(slackIds))
	for _, s := range slackIds {
		args = append(args, s)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	q := fmt.Sprintf(`
		SELECT source_id, onecamp_uuid FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND source_id IN (%s)`,
		joinComma(placeholders))

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]uuid.UUID, len(slackIds))
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

// IdMappingsByType iterates the mapping for an entity type. Used during
// rollback to walk every imported entity. Returns a slice of
// (slackId, onecampUUID) pairs paged in batches of 500.
type IdMapEntry struct {
	SlackId     string
	OnecampUUID uuid.UUID
}

func IdMappingsByType(ctx context.Context, importId uuid.UUID, entityType string) ([]IdMapEntry, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT source_id, onecamp_uuid FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2
		ORDER BY source_id`, importId, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IdMapEntry, 0, 64)
	for rows.Next() {
		var e IdMapEntry
		if err := rows.Scan(&e.SlackId, &e.OnecampUUID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Errors ---------------------------------------------------------------

// LogImportError persists a single error/warning/fatal entry. Workers
// call this for every skipped item so the operator can review the
// coverage report.
func LogImportError(ctx context.Context, importId uuid.UUID, chunkId *uuid.UUID,
	entityType, slackId, severity, code, message string, errorContext json.RawMessage) {
	jobs.LogImportError(ctx, importId, chunkId, entityType, slackId, severity, code, message, errorContext)
}

// ImportError is one entry LogImportErrors writes.
type ImportError = jobs.ImportError

// LogImportErrors is LogImportError for several entries in one statement.
func LogImportErrors(ctx context.Context, importId uuid.UUID, chunkId *uuid.UUID, entries []ImportError) {
	jobs.LogImportErrors(ctx, importId, chunkId, entries)
}

// ListErrors returns up to limit error rows, optionally filtered by severity.
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
			slackId    *string
			sev        string
			code       *string
			msg        string
			contextRaw []byte
			createdAt  time.Time
		)
		if err := rows.Scan(&id, &entityType, &slackId, &sev, &code, &msg, &contextRaw, &createdAt); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id":          id,
			"entity_type": entityType,
			"slack_id":    slackId,
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

// --- helpers --------------------------------------------------------------

// joinComma is a tiny helper to keep CreateChunks readable; strings.Join
// would force an extra import we don't need elsewhere.
func joinComma(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out += "," + parts[i]
	}
	return out
}

// isUniqueViolation detects PG error code 23505 from any driver error
// without requiring the pgx-specific or lib/pq-specific types here.
// We check the error text rather than the code directly to stay portable
// across the two drivers wired into this project.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "duplicate key value") || contains(s, "23505")
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Exec is a passthrough for raw queries used by other packages in the
// import pipeline (e.g., backdating posts after creation). Centralised
// here so we keep all SlackImport-related SQL inside the model layer.
func Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return jobs.Exec(ctx, query, args...)
}

// UpsertIdMappingThenFinish atomically writes the id_map row and marks
// the chunk done. Used by the file worker which has exactly one logical
// item per chunk; coupling these into one tx eliminates a window where
// a crash could leave the file uploaded but un-mapped (orphan blob).
func UpsertIdMappingThenFinish(ctx context.Context, importId, chunkId uuid.UUID,
	entityType, slackId string, onecampUUID uuid.UUID, metadata json.RawMessage) error {
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
		importId, entityType, slackId, onecampUUID, metadata); err != nil {
		return err
	}

	if _, err := tx.ExecContext(dbCtx, `
		UPDATE import_chunks
		SET status = 'done', items_done = 1, error = NULL, updated_at = NOW()
		WHERE id = $1`, chunkId); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// ─── Dedup support (migration 60) ────────────────────────────────────────

// FindCompletedJobByHash returns a previously completed/running job for
// the same workspace with the same content_hash, or nil if none exists.
// Callers use this to short-circuit a duplicate upload before a single
// row of work is done.
//
// We intentionally include 'running' jobs in the lookup so the operator
// gets a clear "this import is already in progress" instead of starting
// a parallel one. The active-per-workspace partial unique index would
// also catch this, but a hash-keyed message is more actionable.
func FindCompletedJobByHash(ctx context.Context, workspaceName, contentHash string) (*Job, error) {
	if contentHash == "" {
		return nil, nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT id, source_workspace_name, source, raw_object_key, status, stage,
		       started_at, completed_at, options, plan, progress,
		       error_message, digest, triggered_by, created_at, updated_at
		FROM import_jobs
		WHERE provider = 'slack'
		  AND source_workspace_name = $1
		  AND content_hash = $2
		  AND status IN ('running','paused','completed','validating','planned')
		ORDER BY created_at DESC
		LIMIT 1`, workspaceName, contentHash)

	j := &Job{}
	err := row.Scan(
		&j.Id, &j.SlackWorkspaceName, &j.Source, &j.RawObjectKey, &j.Status, &j.Stage,
		&j.StartedAt, &j.CompletedAt, &j.Options, (*[]byte)(&j.Plan), &j.Progress,
		&j.ErrorMessage, &j.Digest, &j.TriggeredBy, &j.CreatedAt, &j.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// SetContentHash stores the SHA-256 of the staged ZIP on the job. Called
// once during the validating → planned transition.
func SetContentHash(ctx context.Context, jobId uuid.UUID, hash string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE import_jobs SET content_hash = $2, updated_at = NOW() WHERE id = $1`,
		jobId, hash)
	return err
}

// LookupWorkspaceMapping is the single-key version of the batch helper.
// Returns uuid.Nil and nil error when not found so callers can branch
// cleanly without checking sql.ErrNoRows.
func LookupWorkspaceMapping(ctx context.Context, workspaceName, entityType, slackId string) (uuid.UUID, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT onecamp_uuid FROM import_workspace_id_map
		WHERE provider = 'slack' AND source_workspace_name = $1 AND entity_type = $2 AND source_id = $3`,
		workspaceName, entityType, slackId)
	var u uuid.UUID
	err := row.Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return u, err
}

// UpsertWorkspaceMapping records the workspace-wide mapping. Called by
// resolvers/workers right after they UpsertIdMapping into the per-import
// map. Idempotent on the composite primary key, so re-imports become
// no-ops as expected.
func UpsertWorkspaceMapping(ctx context.Context, workspaceName, entityType, slackId string,
	onecampUUID uuid.UUID, originImportId uuid.UUID) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_workspace_id_map
		    (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
		VALUES ('slack', $1, $2, $3, $4, $5)
		ON CONFLICT (provider, source_workspace_name, entity_type, source_id) DO NOTHING`,
		workspaceName, entityType, slackId, onecampUUID, originImportId)
	return err
}

// UpsertIdMappingWithOwnership extends UpsertIdMapping with the
// created_by_this_import flag. Workers call this when reusing an entity
// from a prior import (created_by_this_import=false), so rollback can
// distinguish ownership.
func UpsertIdMappingWithOwnership(ctx context.Context, importId uuid.UUID,
	entityType, slackId string, onecampUUID uuid.UUID, parentSlackId *string,
	metadata json.RawMessage, createdByThisImport bool) error {
	return jobs.UpsertIdMappingWithOwnership(ctx, importId, entityType, slackId, onecampUUID, parentSlackId, metadata, createdByThisImport)
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
		if err := rows.Scan(&e.SlackId, &e.OnecampUUID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountChannelPosts counts a channel's live posts: all of them, and those
// written by someone other than author. Used to tell a workspace's seeded
// #general (one post, by the admin who made it) from one the team uses.
func CountChannelPosts(ctx context.Context, channelUUID, author uuid.UUID) (total int, others int, err error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE created_by IS DISTINCT FROM $2)
		FROM posts
		WHERE post_channel = $1 AND deleted_at IS NULL`, channelUUID, author).Scan(&total, &others)
	return total, others, err
}

// ─── Cleanup support ─────────────────────────────────────────────────────

// ClearRawObjectKey nullifies raw_object_key on a job. Called after the
// staged ZIP has been deleted from MinIO (either by the cleanup loop,
// rollback, or the manual delete endpoint).
func ClearRawObjectKey(ctx context.Context, jobId uuid.UUID) error {
	return jobs.ClearRawObjectKey(ctx, jobId)
}

// ResetChunkForRetry puts a claimed chunk back in 'pending' WITHOUT
// bumping its attempts counter. Used by workers that encountered a
// transient condition (rate limit, retry-after) where bumping attempts
// would unfairly burn the chunk's retry budget.
//
// This also clears the claim so another worker can pick it up
// immediately rather than waiting for the reaper's stale-claim window.
func ResetChunkForRetry(ctx context.Context, chunkId uuid.UUID, reason string) error {
	return jobs.ResetChunkForRetry(ctx, chunkId, reason)
}

// LookupSlackEmoji translates a Slack emoji shortcode to a OneCamp
// emoji_id via the slack_emoji_map table. Returns ("", nil) when the
// emoji isn't mapped — caller decides whether to log a warning or fall
// back to a default. The lookup is case-insensitive on the Slack name
// because Slack normalises to lowercase but old exports occasionally
// contain mixed case.
//
// We also strip the `:skin-tone-N` suffix as a fallback if the literal
// lookup misses. So `thumbsup::skin-tone-3` first looks up the raw
// string, then `thumbsup` as a fallback.
func LookupSlackEmoji(ctx context.Context, slackName string) (string, error) {
	if slackName == "" {
		return "", nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	lower := strings.ToLower(slackName)
	// Direct lookup.
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
		`SELECT emoji_id FROM slack_emoji_map WHERE slack_name = $1`, lower)
	var emojiId string
	err := row.Scan(&emojiId)
	if err == nil {
		return emojiId, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// Skin-tone fallback. Slack format is "<base>::skin-tone-N".
	if idx := strings.Index(lower, "::"); idx > 0 {
		base := lower[:idx]
		row = postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx,
			`SELECT emoji_id FROM slack_emoji_map WHERE slack_name = $1`, base)
		err = row.Scan(&emojiId)
		if err == nil {
			return emojiId, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", nil
}

// GetIdMapMetadata returns the JSONB metadata blob attached to an
// id_map row. Returns nil and nil error when the row is missing or has
// no metadata. Used by the DM/MPIM message worker to recover the
// grouping_id and member list saved at resolve time.
func GetIdMapMetadata(ctx context.Context, importId uuid.UUID, entityType, slackId string) (json.RawMessage, error) {
	return jobs.GetIdMapMetadata(ctx, importId, entityType, slackId)
}

// (DM/MPIM chunk types are declared at the top of this file alongside
// the channel chunk types: ChunkDMMessages.)

// UncheckedImport is a finished Slack import whose replies the repair of
// misfiled replies hasn't checked yet: its id, its workspace's label and its
// export, if it is still kept.
type UncheckedImport struct {
	Id           uuid.UUID
	Workspace    string
	RawObjectKey *string
}

// UncheckedImports is the unchecked imports, oldest first, at most limit
// (business/SlackImport repair.go).
func UncheckedImports(ctx context.Context, limit int) ([]UncheckedImport, error) {
	return pgexec.Query(ctx, `SELECT id, source_workspace_name, raw_object_key FROM import_jobs
		WHERE provider = 'slack' AND replies_checked_at IS NULL
		  AND status IN ('completed', 'failed', 'cancelled', 'rolled_back')
		ORDER BY created_at
		LIMIT $1`, func(r pgexec.Row) (UncheckedImport, error) {
		var x UncheckedImport
		return x, r.Scan(&x.Id, &x.Workspace, &x.RawObjectKey)
	}, limit)
}

// MarkRepliesChecked records that the repair has checked an import's replies:
// its export may go now.
func MarkRepliesChecked(ctx context.Context, jobId uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE import_jobs SET replies_checked_at = NOW() WHERE id = $1`, jobId)
	return err
}

// ReplyRow is a reply an import made: its key ("<conversation>/<ts>", or a
// bare ts written before keys named the conversation) and what it became.
type ReplyRow struct {
	Key     string
	Comment uuid.UUID
}

// RepliesMade is every reply the import made (not one it found an earlier
// import had made).
func RepliesMade(ctx context.Context, importId uuid.UUID) ([]ReplyRow, error) {
	return pgexec.Query(ctx, `SELECT source_id, onecamp_uuid FROM import_id_map
		WHERE import_id = $1 AND entity_type = 'comment' AND created_by_this_import`,
		func(r pgexec.Row) (ReplyRow, error) {
			var x ReplyRow
			return x, r.Scan(&x.Key, &x.Comment)
		}, importId)
}

// ForgetMapped drops every mapping, the import's and the workspace's, to what
// became onecampUUID, so an import of the same export finds it missing and
// brings it again.
func ForgetMapped(ctx context.Context, importId uuid.UUID, entityType string, onecampUUID uuid.UUID) error {
	if _, err := pgexec.Exec(ctx, `DELETE FROM import_id_map WHERE import_id = $1 AND entity_type = $2 AND onecamp_uuid = $3`,
		importId, entityType, onecampUUID); err != nil {
		return err
	}
	_, err := pgexec.Exec(ctx, `DELETE FROM import_workspace_id_map WHERE provider = 'slack' AND entity_type = $1 AND onecamp_uuid = $2`,
		entityType, onecampUUID)
	return err
}

// MessageAt is the one live message a conversation holds at at, an imported
// message being written at its Slack time: a channel's post (place is its
// uuid) or a DM's or group's chat (place is its grouping id). uuid.Nil for
// none, or for more than one.
func MessageAt(ctx context.Context, place string, channel bool, at time.Time) (uuid.UUID, error) {
	q := `SELECT id FROM chats WHERE grp_id = $1 AND created_at = $2 AND deleted_at IS NULL LIMIT 2`
	if channel {
		q = `SELECT id FROM posts WHERE post_channel = $1::uuid AND created_at = $2 AND deleted_at IS NULL LIMIT 2`
	}
	ids, err := pgexec.Query(ctx, q, func(r pgexec.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, r.Scan(&id)
	}, place, at)
	if err != nil || len(ids) != 1 {
		return uuid.Nil, err
	}
	return ids[0], nil
}
