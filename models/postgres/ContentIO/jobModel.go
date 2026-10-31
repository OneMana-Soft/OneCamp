// Package models keeps the docs programme's import and export jobs
// (content_io_jobs, migration 207). SQL only: what a job does and when is
// business/ContentIO's.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Statuses, as the CHECK constraint has them (migration 207; ready since
// the content_io_ready migration).
const (
	StatusUploading = "uploading"
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusReady     = "ready"
	StatusDone      = "done"
	StatusFailed    = "failed"
)

// Job is one row.
type Job struct {
	ID         uuid.UUID
	Kind       string
	Format     string
	UserUUID   uuid.UUID
	Status     string
	Progress   int
	Params     json.RawMessage
	Result     json.RawMessage
	InputKey   *string
	OutputKey  *string
	Error      *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

const cols = `id, kind, format, user_uuid, status, progress, params, result,
	input_key, output_key, error, created_at, updated_at, finished_at`

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*Job, error) {
	var j Job
	var params, result []byte
	if err := row.Scan(&j.ID, &j.Kind, &j.Format, &j.UserUUID, &j.Status, &j.Progress, &params, &result,
		&j.InputKey, &j.OutputKey, &j.Error, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	j.Params, j.Result = params, result
	return &j, nil
}

func db() (*sql.DB, time.Duration, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, 0, errors.New("content io jobs: no database")
	}
	return postgresInit.DBConn.SqlDB, postgresInit.DBConn.DBTimeout, nil
}

func jsonOrEmpty(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// Create writes a new job. Its id comes from the caller.
func Create(ctx context.Context, j *Job) error {
	conn, timeout, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = conn.ExecContext(ctx, `INSERT INTO content_io_jobs
		(id, kind, format, user_uuid, status, progress, params, result, input_key, output_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 0, $6, '{}'::jsonb, $7, $8, NOW(), NOW())`,
		j.ID, j.Kind, j.Format, j.UserUUID, j.Status, jsonOrEmpty(j.Params), j.InputKey, j.OutputKey)
	return err
}

// Get reads one job; nil when there is none.
func Get(ctx context.Context, id uuid.UUID) (*Job, error) {
	conn, timeout, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	j, err := scan(conn.QueryRowContext(ctx, `SELECT `+cols+` FROM content_io_jobs WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// Queue moves a job that was waiting for its upload into the queue. It
// reports whether the job was still waiting.
func Queue(ctx context.Context, id uuid.UUID, inputKey string) (bool, error) {
	conn, timeout, err := db()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := conn.ExecContext(ctx, `UPDATE content_io_jobs
		SET status = 'queued', input_key = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'uploading'`, id, inputKey)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClaimNext takes the oldest queued job and marks it running; nil when the
// queue is empty. Two workers never take the same job (SKIP LOCKED).
func ClaimNext(ctx context.Context) (*Job, error) {
	conn, timeout, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	j, err := scan(conn.QueryRowContext(ctx, `UPDATE content_io_jobs SET status = 'running', updated_at = NOW()
		WHERE id = (
			SELECT id FROM content_io_jobs WHERE status = 'queued'
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+cols))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// SetProgress records how far a running job has got, 0–100.
func SetProgress(ctx context.Context, id uuid.UUID, progress int) error {
	conn, timeout, err := db()
	if err != nil {
		return err
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = conn.ExecContext(ctx, `UPDATE content_io_jobs SET progress = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'running'`, id, progress)
	return err
}

// Finish ends a job: done with its result, or failed with its error (the
// result then says what was done before it failed).
func Finish(ctx context.Context, id uuid.UUID, status string, result json.RawMessage, outputKey *string, errText *string) error {
	if status != StatusDone && status != StatusFailed {
		return fmt.Errorf("content io jobs: a job ends done or failed, not %q", status)
	}
	conn, timeout, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	progress := "progress"
	if status == StatusDone {
		progress = "100"
	}
	_, err = conn.ExecContext(ctx, `UPDATE content_io_jobs
		SET status = $2, progress = `+progress+`, result = $3, output_key = COALESCE($4, output_key),
		    error = $5, updated_at = NOW(), finished_at = NOW()
		WHERE id = $1`, id, status, jsonOrEmpty(result), outputKey, errText)
	return err
}

// Ready leaves a running job waiting for its person to choose (ready), with
// result, what its file was found to hold.
func Ready(ctx context.Context, id uuid.UUID, result json.RawMessage) error {
	_, err := pgexec.Exec(ctx, `UPDATE content_io_jobs
		SET status = 'ready', progress = 100, result = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'running'`, id, jsonOrEmpty(result))
	return err
}

// Choose queues a ready job again, asked params (holding its person's
// choices), its result cleared. It reports whether the job was still ready.
func Choose(ctx context.Context, id uuid.UUID, params json.RawMessage) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE content_io_jobs
		SET status = 'queued', params = $2, result = '{}'::jsonb, progress = 0, updated_at = NOW()
		WHERE id = $1 AND status = 'ready'`, id, jsonOrEmpty(params))
	return n == 1, err
}

// FailAbandoned fails the jobs left running, waiting for an upload, or ready
// and never chosen for, since before the given times, saying why: a restart
// stopped them, the upload never came, nobody confirmed what to make. It
// returns them, so their stored files can go.
func FailAbandoned(ctx context.Context, runningBefore, uploadingBefore, readyBefore time.Time) ([]*Job, error) {
	conn, timeout, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `UPDATE content_io_jobs
		SET status = 'failed', updated_at = NOW(), finished_at = NOW(),
		    error = CASE
		        WHEN status = 'running' AND kind = 'export' THEN 'The server restarted while this export was being made. Start it again.'
		        WHEN status = 'running' THEN 'The server restarted while this was being imported. Docs made before it stopped are kept; import the rest again.'
		        WHEN status = 'ready' THEN 'Nobody confirmed what to make of this file in time. Import it again.'
		        ELSE 'The file never finished uploading.'
		    END
		WHERE (status = 'running' AND updated_at < $1) OR (status = 'uploading' AND updated_at < $2)
		   OR (status = 'ready' AND updated_at < $3)
		RETURNING `+cols, runningBefore, uploadingBefore, readyBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Recent lists a person's jobs of one kind and format, newest first, at most
// limit.
func Recent(ctx context.Context, userUUID uuid.UUID, kind, format string, limit int) ([]*Job, error) {
	conn, timeout, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT `+cols+` FROM content_io_jobs
		WHERE user_uuid = $1 AND kind = $2 AND format = $3
		ORDER BY created_at DESC LIMIT $4`, userUUID, kind, format, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FinishedBefore lists jobs that ended before the given time and still hold
// a stored file, oldest first, at most limit.
func FinishedBefore(ctx context.Context, before time.Time, limit int) ([]*Job, error) {
	conn, timeout, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT `+cols+` FROM content_io_jobs
		WHERE finished_at IS NOT NULL AND finished_at < $1
		  AND (input_key IS NOT NULL OR output_key IS NOT NULL)
		ORDER BY finished_at ASC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ForgetFiles clears a job's stored file keys once the files are gone.
func ForgetFiles(ctx context.Context, id uuid.UUID) error {
	conn, timeout, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = conn.ExecContext(ctx, `UPDATE content_io_jobs SET input_key = NULL, output_key = NULL WHERE id = $1`, id)
	return err
}

// DeleteFinishedBefore removes job rows that ended before the given time.
func DeleteFinishedBefore(ctx context.Context, before time.Time) (int64, error) {
	conn, timeout, err := db()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := conn.ExecContext(ctx, `DELETE FROM content_io_jobs
		WHERE finished_at IS NOT NULL AND finished_at < $1 AND input_key IS NULL AND output_key IS NULL`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
