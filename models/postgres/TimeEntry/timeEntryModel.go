// Package models (TimeEntry) stores time spent on tasks (migration 181).
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Entry is a stretch of time on a task. EndedAt nil is a running timer.
type Entry struct {
	Id          uuid.UUID  `json:"id"`
	TaskUUID    uuid.UUID  `json:"task_uuid"`
	ProjectUUID uuid.UUID  `json:"project_uuid"`
	UserID      uuid.UUID  `json:"user_id"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
	Note        string     `json:"note"`
	Billable    bool       `json:"billable"`
}

// Seconds is how long the entry has run, up to now for a running timer.
func (e *Entry) Seconds(now time.Time) int64 {
	end := now
	if e.EndedAt != nil {
		end = *e.EndedAt
	}
	if d := end.Sub(e.StartedAt); d > 0 {
		return int64(d / time.Second)
	}
	return 0
}

const columns = `id, task_uuid, project_uuid, user_id, started_at, ended_at, note, billable`

type rowScanner interface{ Scan(dest ...any) error }

func scan(r rowScanner) (*Entry, error) {
	var e Entry
	var ended sql.NullTime
	if err := r.Scan(&e.Id, &e.TaskUUID, &e.ProjectUUID, &e.UserID, &e.StartedAt, &ended, &e.Note, &e.Billable); err != nil {
		return nil, err
	}
	if ended.Valid {
		e.EndedAt = &ended.Time
	}
	return &e, nil
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

func list(query string, args ...any) ([]Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// stopRunning ends a person's running timer at now. A timer stopped in the
// second it started still lasts a second, so the row stays valid.
const stopRunning = `UPDATE task_time_entries SET ended_at = GREATEST($2, started_at + interval '1 second')
	WHERE user_id = $1 AND ended_at IS NULL RETURNING ` + columns

// Start runs a timer on a task for a person, stopping the one they had
// running, which it returns (nil when there was none).
func Start(userID, taskID, projectID uuid.UUID, now time.Time) (started, stopped *Entry, err error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stopped, err = scan(tx.QueryRowContext(ctx, stopRunning, userID, now))
	if errors.Is(err, sql.ErrNoRows) {
		stopped, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	started, err = scan(tx.QueryRowContext(ctx, `INSERT INTO task_time_entries (task_uuid, project_uuid, user_id, started_at)
		VALUES ($1, $2, $3, $4) RETURNING `+columns, taskID, projectID, userID, now))
	if err != nil {
		return nil, nil, err
	}
	return started, stopped, tx.Commit()
}

// Stop ends the person's running timer; nil when none was running.
func Stop(userID uuid.UUID, now time.Time) (*Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	e, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, stopRunning, userID, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// Running is the person's running timer, or nil.
func Running(userID uuid.UUID) (*Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	e, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+columns+` FROM task_time_entries WHERE user_id = $1 AND ended_at IS NULL`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// Add records time a person worked, entered by hand.
func Add(e Entry) (*Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `INSERT INTO task_time_entries
		(task_uuid, project_uuid, user_id, started_at, ended_at, note, billable) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+columns, e.TaskUUID, e.ProjectUUID, e.UserID, e.StartedAt, e.EndedAt, e.Note, e.Billable))
}

// Get is one entry, or nil.
func Get(id uuid.UUID) (*Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	e, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM task_time_entries WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// Update changes a finished entry's span, note and billing; only its owner's.
func Update(id, userID uuid.UUID, startedAt, endedAt time.Time, note string, billable bool) (*Entry, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	e, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `UPDATE task_time_entries
		SET started_at = $3, ended_at = $4, note = $5, billable = $6
		WHERE id = $1 AND user_id = $2 AND ended_at IS NOT NULL RETURNING `+columns, id, userID, startedAt, endedAt, note, billable))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// Delete removes one of the person's entries; false when it wasn't theirs.
func Delete(id, userID uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM task_time_entries WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ForTask is a task's entries, newest first.
func ForTask(taskID uuid.UUID) ([]Entry, error) {
	return list(`SELECT `+columns+` FROM task_time_entries WHERE task_uuid = $1 ORDER BY started_at DESC LIMIT 500`, taskID)
}

// MaxReportRows bounds one report; a range past it is cut, and the report says so.
const MaxReportRows = 20000

// ForProject is a project's entries that started in [from, to), oldest first.
func ForProject(projectID uuid.UUID, from, to time.Time) ([]Entry, error) {
	return list(`SELECT `+columns+` FROM task_time_entries WHERE project_uuid = $1 AND started_at >= $2 AND started_at < $3
		ORDER BY started_at LIMIT $4`, projectID, from, to, MaxReportRows+1)
}
