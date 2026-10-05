package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Cycle is one time-boxed stretch of a project's work (migration 178).
type Cycle struct {
	Id           uuid.UUID  `json:"id"`
	ProjectUUID  uuid.UUID  `json:"project_uuid"`
	Number       int        `json:"number"`
	Name         string     `json:"name"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       time.Time  `json:"ends_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	DoneCount    *int       `json:"done_count,omitempty"`
	CarriedCount *int       `json:"carried_count,omitempty"`
}

// ErrOverlap is a cycle that would share days with another of the project's.
var ErrOverlap = errors.New("cycles overlap")

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const columns = `id, project_uuid, number, name, starts_at, ends_at, completed_at, done_count, carried_count`

func scan(row interface{ Scan(...any) error }) (*Cycle, error) {
	var c Cycle
	err := row.Scan(&c.Id, &c.ProjectUUID, &c.Number, &c.Name, &c.StartsAt, &c.EndsAt, &c.CompletedAt, &c.DoneCount, &c.CarriedCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// List is a project's cycles in order.
func List(project uuid.UUID) ([]Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+columns+` FROM project_cycles WHERE project_uuid = $1 ORDER BY number`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cycle{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Get is one cycle, or nil.
func Get(id uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM project_cycles WHERE id = $1`, id))
}

// Create numbers the cycle after the project's last and refuses overlaps,
// under a per-project lock so two cycles made at once can't collide.
func Create(project uuid.UUID, name string, startsAt, endsAt time.Time, by uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cycles:"+project.String()); err != nil {
		return nil, err
	}
	var clash bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM project_cycles
		WHERE project_uuid = $1 AND starts_at < $3 AND ends_at > $2)`, project, startsAt, endsAt).Scan(&clash); err != nil {
		return nil, err
	}
	if clash {
		return nil, ErrOverlap
	}
	c, err := scan(tx.QueryRowContext(ctx, `
		INSERT INTO project_cycles (project_uuid, number, name, starts_at, ends_at, created_by)
		VALUES ($1, COALESCE((SELECT MAX(number) FROM project_cycles WHERE project_uuid = $1), 0) + 1, $2, $3, $4, $5)
		RETURNING `+columns, project, name, startsAt, endsAt, by))
	if err != nil {
		if helpers.IsUniqueViolation(err) {
			return nil, ErrOverlap
		}
		return nil, err
	}
	return c, tx.Commit()
}

// Rename changes a cycle's name.
func Rename(id uuid.UUID, name string) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE project_cycles SET name = $2 WHERE id = $1`, id, name)
	return err
}

// Delete removes a cycle; its tasks simply leave it.
func Delete(id uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM project_cycles WHERE id = $1`, id)
	return err
}

// Members maps each cycle of the project to its tasks.
func Members(project uuid.UUID) (map[uuid.UUID][]string, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT tc.cycle_id, tc.task_uuid FROM task_cycles tc
		  JOIN project_cycles c ON c.id = tc.cycle_id WHERE c.project_uuid = $1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]string{}
	for rows.Next() {
		var c, t uuid.UUID
		if err := rows.Scan(&c, &t); err != nil {
			return nil, err
		}
		out[c] = append(out[c], t.String())
	}
	return out, rows.Err()
}

// TasksIn lists a cycle's tasks.
func TasksIn(cycle uuid.UUID) ([]string, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT task_uuid FROM task_cycles WHERE cycle_id = $1`, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t.String())
	}
	return out, rows.Err()
}

// CycleOf is the cycle a task is in, or nil.
func CycleOf(task uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT `+prefixed+` FROM project_cycles c JOIN task_cycles tc ON tc.cycle_id = c.id WHERE tc.task_uuid = $1`, task))
}

const prefixed = `c.id, c.project_uuid, c.number, c.name, c.starts_at, c.ends_at, c.completed_at, c.done_count, c.carried_count`

// SetTask puts a task in a cycle, or takes it out (cycle nil).
func SetTask(task uuid.UUID, cycle *uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	var err error
	if cycle == nil {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM task_cycles WHERE task_uuid = $1`, task)
	} else {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)
			ON CONFLICT (task_uuid) DO UPDATE SET cycle_id = EXCLUDED.cycle_id, added_at = NOW()`, task, *cycle)
	}
	return err
}

// Complete closes a cycle and moves the listed tasks to the next cycle (or
// out of cycles when next is nil), in one transaction. False when the cycle
// was already complete.
func Complete(id uuid.UUID, done int, carry []string, next *uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `UPDATE project_cycles SET completed_at = NOW(), done_count = $2, carried_count = $3
		WHERE id = $1 AND completed_at IS NULL`, id, done, len(carry))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	for _, t := range carry {
		if next == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM task_cycles WHERE task_uuid = $1 AND cycle_id = $2`, t, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE task_cycles SET cycle_id = $3, added_at = NOW() WHERE task_uuid = $1 AND cycle_id = $2`, t, id, *next)
		}
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
