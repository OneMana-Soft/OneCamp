// Package models is the Archive's Postgres: its policies, its jobs and their
// items, and the soft-deletes it makes and undoes on posts, chats, tasks and
// attachments (backend audit U3: business/Archive ran 34 statements itself).
package models

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ArchivePolicy is one entity type's archive rule.
type ArchivePolicy struct {
	Id                          uuid.UUID `json:"id"`
	EntityType                  string    `json:"entity_type"`
	RetentionDays               int       `json:"retention_days"`
	AutoArchive                 bool      `json:"auto_archive"`
	ArchiveCompletedTasks       bool      `json:"archive_completed_tasks"`
	ArchiveInactiveChannelsDays int       `json:"archive_inactive_channels_days"`
	CompressAttachments         bool      `json:"compress_attachments"`
	// PurgeAfterDays is how long an archived file or recording waits before
	// its stored object is removed for good (0: never).
	PurgeAfterDays int        `json:"purge_after_days"`
	PurgedCount    int64      `json:"purged_count"`
	PurgedBytes    int64      `json:"purged_bytes"`
	LastPurgedAt   *time.Time `json:"last_purged_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ArchiveJob is one run of an archive.
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
	// Undoing is whether its undo is at work now (beingUndone): the card shows
	// it restoring and offers no Undo.
	Undoing bool `json:"undoing,omitempty"`
}

const policyColumns = `id, entity_type, retention_days, auto_archive, archive_completed_tasks, archive_inactive_channels_days, compress_attachments, purge_after_days, purged_count, purged_bytes, last_purged_at, created_at, updated_at`

func scanPolicy(r pgexec.Row) (*ArchivePolicy, error) {
	var p ArchivePolicy
	err := r.Scan(&p.Id, &p.EntityType, &p.RetentionDays, &p.AutoArchive, &p.ArchiveCompletedTasks, &p.ArchiveInactiveChannelsDays, &p.CompressAttachments, &p.PurgeAfterDays, &p.PurgedCount, &p.PurgedBytes, &p.LastPurgedAt, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// ListPolicies is every entity type's policy, by entity type.
func ListPolicies(ctx context.Context) ([]*ArchivePolicy, error) {
	return pgexec.Query(ctx, `SELECT `+policyColumns+` FROM archive_policies ORDER BY entity_type`, scanPolicy)
}

// GetPolicy is one entity type's policy; found false when it has none.
func GetPolicy(ctx context.Context, entityType string) (*ArchivePolicy, bool, error) {
	return pgexec.QueryOne(ctx, `SELECT `+policyColumns+` FROM archive_policies WHERE entity_type = $1`, scanPolicy, entityType)
}

// UpdatePolicy changes the given fields of an entity type's policy (nil
// keeps one), and answers how many policies it changed (0 or 1).
func UpdatePolicy(ctx context.Context, entityType string, retentionDays *int, autoArchive, archiveCompletedTasks *bool,
	archiveInactiveChannelsDays *int, compressAttachments *bool, purgeAfterDays *int) (int64, error) {
	return pgexec.Exec(ctx, `
		UPDATE archive_policies SET
			retention_days = COALESCE($1, retention_days),
			auto_archive = COALESCE($2, auto_archive),
			archive_completed_tasks = COALESCE($3, archive_completed_tasks),
			archive_inactive_channels_days = COALESCE($4, archive_inactive_channels_days),
			compress_attachments = COALESCE($5, compress_attachments),
			purge_after_days = COALESCE($6, purge_after_days),
			updated_at = NOW()
		WHERE entity_type = $7`, retentionDays, autoArchive, archiveCompletedTasks, archiveInactiveChannelsDays, compressAttachments, purgeAfterDays, entityType)
}

// RecordPurge adds what a purge pass removed to the policy's running total.
func RecordPurge(ctx context.Context, entityType string, removed, bytes int64) error {
	_, err := pgexec.Exec(ctx, `
		UPDATE archive_policies
		SET purged_count = purged_count + $1, purged_bytes = purged_bytes + $2, last_purged_at = NOW()
		WHERE entity_type = $3`, removed, bytes, entityType)
	return err
}

// RunningJobExists is whether an archive of this entity type is running.
func RunningJobExists(ctx context.Context, entityType string) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT EXISTS(SELECT 1 FROM archive_jobs WHERE entity_type = $1 AND status = 'running')`, entityType)
}

// CreateJob records a run that has started.
func CreateJob(ctx context.Context, jobId uuid.UUID, entityType string, startedAt time.Time, triggeredBy *uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `
		INSERT INTO archive_jobs (id, entity_type, status, started_at, triggered_by, created_at)
		VALUES ($1, $2, 'running', $3, $4, $3)`, jobId, entityType, startedAt, triggeredBy)
	return err
}

// FinishJob records how a run ended.
func FinishJob(ctx context.Context, jobId uuid.UUID, status string, completedAt time.Time, processed, archived int64, failed int, errMsg *string) error {
	_, err := pgexec.Exec(ctx, `
		UPDATE archive_jobs SET status=$1, completed_at=$2, items_processed=$3, items_archived=$4, items_failed=$5, error_message=$6, metadata=NULL WHERE id=$7`,
		status, completedAt, processed, archived, failed, errMsg, jobId)
	return err
}

// AddJobItems records the ids a run archived, in one statement (and one
// transaction, under pgexec's timeout) however many: it was a statement per
// 1,000 in a transaction with no timeout. Idempotent.
func AddJobItems(ctx context.Context, jobId uuid.UUID, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		INSERT INTO archive_job_items (job_id, entity_id)
		SELECT $1, unnest($2::text[])
		ON CONFLICT DO NOTHING`, jobId, pq.Array(ids))
	return err
}

// JobItems is the ids a run archived.
func JobItems(ctx context.Context, jobId uuid.UUID) ([]string, error) {
	return pgexec.Query(ctx, `SELECT entity_id FROM archive_job_items WHERE job_id = $1`,
		func(r pgexec.Row) (id string, err error) { err = r.Scan(&id); return }, jobId)
}

// UndoJob is what undoing a run needs to know of it; found false for none.
func UndoJob(ctx context.Context, jobId uuid.UUID) (entityType, status string, metadata *string, found bool, err error) {
	type row struct {
		t, s string
		m    *string
	}
	r, found, err := pgexec.QueryOne(ctx, `SELECT entity_type, status, metadata FROM archive_jobs WHERE id = $1`,
		func(rw pgexec.Row) (x row, err error) { err = rw.Scan(&x.t, &x.s, &x.m); return }, jobId)
	return r.t, r.s, r.m, found, err
}

// UndoDeadline bounds an undo once it has begun (business/Archive
// UndoArchiveJob): past it, the undo's own context ends.
const UndoDeadline = 30 * time.Minute

// beingUndone is the SQL for "its undo is at work now": partial, with an undo
// that began less than UndoDeadline ago and hasn't finished. deadline is the
// placeholder that carries UndoDeadline in seconds. An undo that stopped has
// no start (FinishUndo clears it), or an older one when its server went down
// mid-way.
func beingUndone(deadline string) string {
	return `(status = 'partial' AND COALESCE(undo_started_at > NOW() - ` + deadline + `::float8 * INTERVAL '1 second', false))`
}

// ClaimUndo marks a run as being undone ("partial": part of it may be back
// from here on), when it is completed, or partly undone and not being undone
// now. claimed is false otherwise: being undone by another request (see
// UndoUnderWay), undone meanwhile, or never completed.
func ClaimUndo(ctx context.Context, jobId uuid.UUID) (claimed bool, err error) {
	_, claimed, err = pgexec.QueryOne(ctx, `
		UPDATE archive_jobs SET status = 'partial', error_message = NULL, undo_started_at = NOW()
		WHERE id = $1 AND status IN ('completed', 'partial') AND NOT `+beingUndone("$2")+` RETURNING id`,
		func(r pgexec.Row) (id uuid.UUID, err error) { err = r.Scan(&id); return }, jobId, UndoDeadline.Seconds())
	return claimed, err
}

// UndoUnderWay is whether the run's undo is at work now: what a refused claim
// waits for.
func UndoUnderWay(ctx context.Context, jobId uuid.UUID) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT EXISTS(SELECT 1 FROM archive_jobs WHERE id = $1 AND `+beingUndone("$2")+`)`, jobId, UndoDeadline.Seconds())
}

// FinishUndo records how an undo claimed by ClaimUndo ended: undone
// ("cancelled", restored), or, on failure, still partly undone with why, and
// no longer at work, so Undo can finish it.
func FinishUndo(ctx context.Context, jobId uuid.UUID, failure error) error {
	if failure != nil {
		_, err := pgexec.Exec(ctx, `UPDATE archive_jobs SET error_message = $2, undo_started_at = NULL WHERE id = $1 AND status = 'partial'`, jobId, failure.Error())
		return err
	}
	_, err := pgexec.Exec(ctx, `
		UPDATE archive_jobs SET status = 'cancelled', error_message = NULL, undo_started_at = NULL, metadata = jsonb_build_object('restored', true)
		WHERE id = $1 AND status = 'partial'`, jobId)
	return err
}

// ListJobs is the 50 newest runs.
func ListJobs(ctx context.Context) ([]*ArchiveJob, error) {
	return pgexec.Query(ctx, `
		SELECT id, entity_type, status, started_at, completed_at, items_processed, items_archived, items_failed, error_message, triggered_by, created_at,
			`+beingUndone("$1")+`
		FROM archive_jobs ORDER BY created_at DESC LIMIT 50`, func(r pgexec.Row) (*ArchiveJob, error) {
		var j ArchiveJob
		err := r.Scan(&j.Id, &j.EntityType, &j.Status, &j.StartedAt, &j.CompletedAt, &j.ItemsProcessed, &j.ItemsArchived, &j.ItemsFailed, &j.ErrorMessage, &j.TriggeredBy, &j.CreatedAt, &j.Undoing)
		return &j, err
	}, UndoDeadline.Seconds())
}

// ArchivedCounts is how many posts, chats, tasks and attachments are
// soft-deleted, by table.
func ArchivedCounts(ctx context.Context) (map[string]int64, error) {
	return pgexec.Pairs[string, int64](ctx, `
		SELECT 'posts', COUNT(*) FROM posts WHERE deleted_at IS NOT NULL
		UNION ALL SELECT 'chats', COUNT(*) FROM chats WHERE deleted_at IS NOT NULL
		UNION ALL SELECT 'tasks', COUNT(*) FROM tasks WHERE deleted_at IS NOT NULL
		UNION ALL SELECT 'attachments', COUNT(*) FROM attachments WHERE deleted_at IS NOT NULL`)
}

// archivable are the tables the archive soft-deletes, and how it marks one
// (attachments has no updated_at).
var archivable = map[string]string{
	"posts":       "deleted_at = NOW(), updated_at = NOW()",
	"chats":       "deleted_at = NOW(), updated_at = NOW()",
	"tasks":       "deleted_at = NOW(), updated_at = NOW()",
	"attachments": "deleted_at = NOW()",
}

var restoreSet = map[string]string{
	"posts":       "deleted_at = NULL, updated_at = NOW()",
	"chats":       "deleted_at = NULL, updated_at = NOW()",
	"tasks":       "deleted_at = NULL, updated_at = NOW()",
	"attachments": "deleted_at = NULL",
}

// ArchiveBefore soft-deletes a table's live rows made before cutoff and
// answers the ids it took, in one statement: it read the ids and then
// updated by the same predicate, so a row deleted (or restored) between the
// two was listed as archived, and an undo brought it back.
func ArchiveBefore(ctx context.Context, table string, cutoff time.Time) ([]string, error) {
	set, ok := archivable[table]
	if !ok {
		return nil, fmt.Errorf("archive: no such table %q", table)
	}
	return pgexec.Query(ctx, fmt.Sprintf(`UPDATE %s SET %s WHERE created_at < $1 AND deleted_at IS NULL RETURNING id::text`, table, set), pgexec.Text, cutoff)
}

// ArchiveTasksIn soft-deletes these live tasks made before cutoff.
func ArchiveTasksIn(ctx context.Context, cutoff time.Time, ids []string) ([]string, error) {
	return pgexec.Query(ctx, `
		UPDATE tasks SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = ANY($2::uuid[]) AND created_at < $1 AND deleted_at IS NULL
		RETURNING id::text`, pgexec.Text, cutoff, pq.Array(ids))
}

// Restore brings these soft-deleted rows of a table back and answers which.
func Restore(ctx context.Context, table string, ids []string) ([]string, error) {
	set, ok := restoreSet[table]
	if !ok {
		return nil, fmt.Errorf("restore: no such table %q", table)
	}
	return pgexec.Query(ctx, fmt.Sprintf(`UPDATE %s SET %s WHERE id = ANY($1::uuid[]) AND deleted_at IS NOT NULL RETURNING id::text`, table, set), pgexec.Text, pq.Array(ids))
}

// Live is which of these rows of a table are live (not soft-deleted): what a
// restore of them has brought back, by this call or an earlier one.
func Live(ctx context.Context, table string, ids []string) ([]string, error) {
	if !Archivable(table) {
		return nil, fmt.Errorf("restore: no such table %q", table)
	}
	return pgexec.Query(ctx, fmt.Sprintf(`SELECT id::text FROM %s WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, table), pgexec.Text, pq.Array(ids))
}

// Archived is one soft-deleted row and when.
type Archived struct {
	ID string
	At time.Time
}

// RecentlyArchived is a page of a table's rows soft-deleted since `since`,
// newest first, and how many there are in all.
func RecentlyArchived(ctx context.Context, table string, since time.Time, limit, offset int) ([]Archived, int64, error) {
	if _, ok := archivable[table]; !ok {
		return nil, 0, fmt.Errorf("archive: no such table %q", table)
	}
	total, err := pgexec.Scalar[int64](ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE deleted_at IS NOT NULL AND deleted_at > $1`, table), since)
	if err != nil {
		return nil, 0, err
	}
	rows, err := pgexec.Query(ctx, fmt.Sprintf(`SELECT id::text, deleted_at FROM %s WHERE deleted_at IS NOT NULL AND deleted_at > $1 ORDER BY deleted_at DESC LIMIT $2 OFFSET $3`, table),
		func(r pgexec.Row) (a Archived, err error) { err = r.Scan(&a.ID, &a.At); return }, since, limit, offset)
	return rows, total, err
}

// Archivable is whether the archive soft-deletes (and restores) this table.
func Archivable(table string) bool {
	_, ok := archivable[table]
	return ok
}

// CountBefore is how many of a table's live rows were made before cutoff:
// what an archive that failed had set out to take.
func CountBefore(ctx context.Context, table string, cutoff time.Time) (int64, error) {
	if !Archivable(table) {
		return 0, fmt.Errorf("archive: no such table %q", table)
	}
	return pgexec.Scalar[int64](ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE created_at < $1 AND deleted_at IS NULL`, table), cutoff)
}
