package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// TaskRecurrence is how a task repeats (migration 175).
type TaskRecurrence struct {
	TaskUUID  uuid.UUID  `json:"task_uuid"`
	Rule      string     `json:"rule"`
	Mode      string     `json:"mode"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

const (
	ModeSchedule   = "schedule"
	ModeCompletion = "completion"
)

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const columns = `task_uuid, rule, mode, created_by, updated_at`

func scan(row interface{ Scan(...any) error }) (*TaskRecurrence, error) {
	var r TaskRecurrence
	if err := row.Scan(&r.TaskUUID, &r.Rule, &r.Mode, &r.CreatedBy, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// Get returns the task's recurrence, or nil when it doesn't repeat.
func Get(taskUUID uuid.UUID) (*TaskRecurrence, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	r, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+columns+` FROM task_recurrences WHERE task_uuid = $1`, taskUUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Set makes the task repeat, replacing any rule it had.
func Set(taskUUID uuid.UUID, rule, mode string, createdBy uuid.UUID) (*TaskRecurrence, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO task_recurrences (task_uuid, rule, mode, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_uuid) DO UPDATE
		   SET rule = EXCLUDED.rule, mode = EXCLUDED.mode, updated_at = NOW()
		RETURNING `+columns, taskUUID, rule, mode, createdBy))
}

// Delete stops the task repeating.
func Delete(taskUUID uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM task_recurrences WHERE task_uuid = $1`, taskUUID)
	return err
}

// Take removes and returns the task's recurrence in one statement, so a task
// completed twice at once (two tabs, an agent and a person) makes one next
// occurrence, not two. Nil when it doesn't repeat (or was already taken).
func Take(taskUUID uuid.UUID) (*TaskRecurrence, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	r, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`DELETE FROM task_recurrences WHERE task_uuid = $1 RETURNING `+columns, taskUUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Put stores a recurrence on a task as it was (moving it to the next
// occurrence, or back when making that occurrence failed).
func Put(r *TaskRecurrence) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO task_recurrences (task_uuid, rule, mode, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_uuid) DO NOTHING`, r.TaskUUID, r.Rule, r.Mode, r.CreatedBy)
	return err
}
