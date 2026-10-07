// Package models (ProjectTemplate) stores the project templates a workspace
// saves (migration 186). The template itself is business/ProjectTemplate's to
// read and check; here it is a JSON body beside what a list shows.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrNameTaken: another live template has that name, in any case.
var ErrNameTaken = errors.New("a template with that name already exists")

// Template is one saved template. Body is left out of a list.
type Template struct {
	ID          uuid.UUID
	Name        string
	Description string
	Body        []byte
	TaskCount   int
	Preview     []string
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// Add stores a new template and fills in its id and creation time.
func Add(ctx context.Context, t *Template) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO project_templates (name, description, body, task_count, preview, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		t.Name, t.Description, t.Body, t.TaskCount, pq.Array(t.Preview), t.CreatedBy).Scan(&t.ID, &t.CreatedAt)
	if helpers.IsUniqueViolation(err) {
		return ErrNameTaken
	}
	return err
}

// List is the live templates, newest first, at most limit, without bodies.
func List(ctx context.Context, limit int) ([]Template, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT id, name, description, task_count, preview, created_by, created_at FROM project_templates
		 WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.TaskCount, pq.Array(&t.Preview), &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Count is how many live templates there are.
func Count(ctx context.Context) (n int, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM project_templates WHERE deleted_at IS NULL`).Scan(&n)
	return
}

// Get is one live template with its body, or nil.
func Get(ctx context.Context, id uuid.UUID) (*Template, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var t Template
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT id, name, description, body, task_count, preview, created_by, created_at FROM project_templates
		 WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&t.ID, &t.Name, &t.Description, &t.Body, &t.TaskCount, pq.Array(&t.Preview), &t.CreatedBy, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Delete removes a template; false when it was already gone.
func Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE project_templates SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
