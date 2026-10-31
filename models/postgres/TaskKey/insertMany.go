package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// InsertTasks is InsertTask for many tasks of one project, in one statement:
// their rows, numbered in the order given after the project's last (the key's
// row lock taken once for them all). A project with no key yet gives them no
// number (0, for the projects.keys sweep).
func InsertTasks(ctx context.Context, ids []uuid.UUID, projectID, createdBy uuid.UUID, createdAt time.Time) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if !connected() {
		return nil, ErrNoDatabase
	}
	type row struct {
		id uuid.UUID
		n  sql.NullInt64
	}
	rows, err := pgexec.Query(ctx, `
		WITH k AS (
			UPDATE project_keys SET next_number = next_number + $5
			WHERE project_id = $2
			RETURNING next_number - $5 AS first
		)
		INSERT INTO tasks (id, project_id, created_by, created_at, updated_at, task_number)
		SELECT t.id, $2, $3, $4, $4, (SELECT first FROM k) + t.n - 1
		FROM unnest($1::uuid[]) WITH ORDINALITY AS t(id, n)
		RETURNING id, task_number`,
		func(r pgexec.Row) (row, error) {
			var x row
			err := r.Scan(&x.id, &x.n)
			return x, err
		}, pq.Array(ids), projectID, createdBy, createdAt, len(ids))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.id] = int(x.n.Int64)
	}
	return out, nil
}

// InsertTaskNumbered is InsertTask keeping a number the task had in another
// tool (an import: ENG-123 stays 123): the number when the project has no task
// with it yet, and the project's next number when one does. The project's
// next number then follows the highest. 0 when the project has no key yet.
// typeID is the task's type (nil: a Task), set as the row is made.
func InsertTaskNumbered(ctx context.Context, taskID, projectID, createdBy uuid.UUID, createdAt time.Time, number int, typeID *uuid.UUID) (int, error) {
	if !connected() {
		return 0, ErrNoDatabase
	}
	if number < 1 {
		return insertTask(ctx, taskID, projectID, createdBy, createdAt, typeID)
	}
	var got int
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		// The key's row lock, as InsertTask takes it, so numbers stay unique.
		next, err := pgexec.Scalar[int](ctx, `SELECT next_number FROM project_keys WHERE project_id = $1 FOR UPDATE`, projectID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = pgexec.Exec(ctx, `INSERT INTO tasks (id, project_id, created_by, created_at, updated_at, task_type_id) VALUES ($1, $2, $3, $4, $4, $5)`, taskID, projectID, createdBy, createdAt, typeID)
			return err
		}
		if err != nil {
			return err
		}
		taken, err := pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE project_id = $1 AND task_number = $2)`, projectID, number)
		if err != nil {
			return err
		}
		got = number
		if taken {
			got = next
		}
		if _, err := pgexec.Exec(ctx, `INSERT INTO tasks (id, project_id, created_by, created_at, updated_at, task_number, task_type_id) VALUES ($1, $2, $3, $4, $4, $5, $6)`,
			taskID, projectID, createdBy, createdAt, got, typeID); err != nil {
			return err
		}
		return NumberAfter(ctx, projectID, got)
	})
	return got, err
}

// NumberAfter makes the next number a project gives a task come after n, when
// it doesn't already: a task that kept n moves the numbering past it, and an
// import keeps every number its source's tasks hold free before it makes a
// task with none (business/Import keepNumbersFree). A project with no key yet
// is left to the task that settles it.
func NumberAfter(ctx context.Context, projectID uuid.UUID, n int) error {
	if !connected() {
		return ErrNoDatabase
	}
	_, err := pgexec.Exec(ctx, `UPDATE project_keys SET next_number = GREATEST(next_number, $2 + 1) WHERE project_id = $1`, projectID, n)
	return err
}
