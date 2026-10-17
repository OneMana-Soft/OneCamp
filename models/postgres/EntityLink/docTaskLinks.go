// Package models (EntityLink) keeps which docs hold a to-do for which task
// (migration 340, doc_task_links): what ticks a doc's box when its task is done
// elsewhere. A row is written only by the conversion that made the task
// (business/Task/taskDocSync.go), and removed when a change to the task finds
// the doc no longer holds its to-do.
package models

import (
	"context"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AddDocTask records that a doc holds a to-do for a task. Recording it again
// changes nothing.
func AddDocTask(ctx context.Context, docUUID, taskUUID, by uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`INSERT INTO doc_task_links (doc_uuid, task_uuid, created_by) VALUES ($1, $2, $3)
		 ON CONFLICT (doc_uuid, task_uuid) DO NOTHING`, docUUID, taskUUID, by)
	return err
}

// HasDocTask is whether a doc holds a to-do for a task.
func HasDocTask(ctx context.Context, docUUID, taskUUID uuid.UUID) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT count(*) FROM doc_task_links WHERE doc_uuid = $1 AND task_uuid = $2`, docUUID, taskUUID).Scan(&n)
	return n > 0, err
}

// DocsForTask is every doc that holds a to-do for a task.
func DocsForTask(ctx context.Context, taskUUID uuid.UUID) ([]uuid.UUID, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT doc_uuid FROM doc_task_links WHERE task_uuid = $1 ORDER BY created_at`, taskUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RemoveDocTask records that a doc no longer holds a to-do for a task.
func RemoveDocTask(ctx context.Context, docUUID, taskUUID uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM doc_task_links WHERE doc_uuid = $1 AND task_uuid = $2`, docUUID, taskUUID)
	return err
}
