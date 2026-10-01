package GitHubImportJob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Job kinds. Closed enum at the application layer (see migration
// note); add new kinds here as you implement them.
const (
	KindIssues = "issues"
	KindPRs    = "prs"
)

// Status values match the SQL DEFAULT and the worker's transitions.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Job is the in-memory shape of a single import row. Times are
// nullable because rows transition through multiple states.
type Job struct {
	Id            uuid.UUID
	LinkId        uuid.UUID
	ImportKind    string
	TriggeredBy   *uuid.UUID
	Status        string
	ItemsTotal    int
	ItemsImported int
	ItemsSkipped  int
	ItemsFailed   int
	ErrorMessage  *string
	StartedAt     *time.Time
	CompletedAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const cols = `id, link_id, import_kind, triggered_by, status,
	items_total, items_imported, items_skipped, items_failed,
	error_message, started_at, completed_at, created_at, updated_at`

func scanJob(row interface {
	Scan(dest ...interface{}) error
}) (*Job, error) {
	var j Job
	err := row.Scan(
		&j.Id, &j.LinkId, &j.ImportKind, &j.TriggeredBy, &j.Status,
		&j.ItemsTotal, &j.ItemsImported, &j.ItemsSkipped, &j.ItemsFailed,
		&j.ErrorMessage, &j.StartedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// Create inserts a new import job row in 'pending' status. The caller
// signals the worker via a non-blocking channel send; this function
// only writes the row.
func Create(ctx context.Context, linkId uuid.UUID, kind string, triggeredBy *uuid.UUID) (*Job, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	id := uuid.New()
	now := time.Now()

	query := `INSERT INTO github_import_jobs (id, link_id, import_kind, triggered_by, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, $5)`

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2, query, id, linkId, kind, triggeredBy, now); err != nil {
		return nil, err
	}

	return GetById(ctx, id)
}

// GetById loads a single job by its UUID.
func GetById(ctx context.Context, id uuid.UUID) (*Job, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := fmt.Sprintf("SELECT %s FROM github_import_jobs WHERE id = $1", cols)
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx2, q, id)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// ListByLink returns the most recent N jobs for a link.
func ListByLink(ctx context.Context, linkId uuid.UUID, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := fmt.Sprintf(`SELECT %s FROM github_import_jobs WHERE link_id = $1 ORDER BY created_at DESC LIMIT $2`, cols)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx2, q, linkId, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClaimNext atomically claims the next pending job and flips it to
// 'running'. Returns nil when nothing is pending. Uses
// FOR UPDATE SKIP LOCKED so multiple workers (or replicas) never
// claim the same row.
func ClaimNext(ctx context.Context) (*Job, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx2, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	pickQuery := fmt.Sprintf(`SELECT %s FROM github_import_jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, cols)

	row := tx.QueryRowContext(ctx2, pickQuery)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now()
	if _, err := tx.ExecContext(ctx2,
		`UPDATE github_import_jobs SET status = 'running', started_at = $1, updated_at = $1 WHERE id = $2`,
		now, job.Id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	job.Status = StatusRunning
	job.StartedAt = &now
	return job, nil
}

// UpdateProgress writes incremental counters and bumps updated_at so
// the admin UI can show live progress without waiting for completion.
//
// Called from the worker every batch of issues/PRs (typically once
// per GitHub API page) to amortise DB writes.
func UpdateProgress(ctx context.Context, id uuid.UUID, total, imported, skipped, failed int) error {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2,
		`UPDATE github_import_jobs SET
			items_total = $1, items_imported = $2, items_skipped = $3, items_failed = $4,
			updated_at = NOW()
		WHERE id = $5`,
		total, imported, skipped, failed, id)
	return err
}

// Finalize transitions the row to a terminal state. Pass empty errMsg
// for success.
func Finalize(ctx context.Context, id uuid.UUID, success bool, total, imported, skipped, failed int, errMsg string) error {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	status := StatusCompleted
	var errPtr *string
	if !success {
		status = StatusFailed
		if errMsg != "" {
			errPtr = &errMsg
		}
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2,
		`UPDATE github_import_jobs SET
			status = $1, items_total = $2, items_imported = $3, items_skipped = $4, items_failed = $5,
			error_message = $6, completed_at = NOW(), updated_at = NOW()
		WHERE id = $7`,
		status, total, imported, skipped, failed, errPtr, id)
	return err
}

// ReapStale rolls running rows back to pending when they have not
// been touched in `staleAfter`. Catches worker crashes mid-import.
func ReapStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	cutoff := time.Now().Add(-staleAfter)
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2,
		`UPDATE github_import_jobs SET status = 'pending', updated_at = NOW()
		WHERE status = 'running' AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CleanupOld removes terminal rows older than retention. Bounded
// growth: a busy workspace might run a few imports a day, so 30d of
// rows is at most a few hundred.
func CleanupOld(ctx context.Context, retention time.Duration) (int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	cutoff := time.Now().Add(-retention)
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2,
		`DELETE FROM github_import_jobs
		WHERE status IN ('completed', 'failed') AND completed_at IS NOT NULL AND completed_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
