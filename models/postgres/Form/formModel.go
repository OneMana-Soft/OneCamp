package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Form is a project's intake form (migration 179).
type Form struct {
	Id           uuid.UUID       `json:"id"`
	ProjectUUID  uuid.UUID       `json:"project_uuid"`
	Token        string          `json:"token"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Fields       json.RawMessage `json:"fields"`
	TitleField   string          `json:"title_field"`
	Priority     string          `json:"priority"`
	AssigneeUUID *uuid.UUID      `json:"assignee_uuid,omitempty"`
	Active       bool            `json:"active"`
	CreatedBy    uuid.UUID       `json:"created_by"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Submissions  int             `json:"submissions"`
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const columns = `f.id, f.project_uuid, f.token, f.title, f.description, f.fields, f.title_field, f.priority,
	f.assignee_uuid, f.active, f.created_by, f.updated_at,
	(SELECT COUNT(*) FROM form_submissions s WHERE s.form_id = f.id)`

func scan(row interface{ Scan(...any) error }) (*Form, error) {
	var f Form
	err := row.Scan(&f.Id, &f.ProjectUUID, &f.Token, &f.Title, &f.Description, &f.Fields, &f.TitleField, &f.Priority,
		&f.AssigneeUUID, &f.Active, &f.CreatedBy, &f.UpdatedAt, &f.Submissions)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// List is a project's forms, newest first.
func List(project uuid.UUID) ([]Form, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+columns+` FROM project_forms f WHERE f.project_uuid = $1 ORDER BY f.created_at DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Form{}
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// Count is how many forms a project has.
func Count(project uuid.UUID) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_forms WHERE project_uuid = $1`, project).Scan(&n)
	return n, err
}

// ByToken is the form a public link points at, or nil.
func ByToken(token string) (*Form, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.token = $1`, token))
}

// Get is a form of the project, or nil.
func Get(project, id uuid.UUID) (*Form, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+columns+` FROM project_forms f WHERE f.id = $1 AND f.project_uuid = $2`, id, project))
}

// Create stores a new form.
func Create(f Form) (*Form, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var id uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO project_forms (project_uuid, token, title, description, fields, title_field, priority, assignee_uuid, active, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		f.ProjectUUID, f.Token, f.Title, f.Description, []byte(f.Fields), f.TitleField, f.Priority, f.AssigneeUUID, f.Active, f.CreatedBy).Scan(&id)
	if err != nil {
		return nil, err
	}
	return Get(f.ProjectUUID, id)
}

// Update changes a form's content; its link and owner stay.
func Update(f Form) (*Form, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE project_forms SET title = $3, description = $4, fields = $5, title_field = $6, priority = $7,
		       assignee_uuid = $8, active = $9, created_by = $10, updated_at = NOW()
		 WHERE id = $1 AND project_uuid = $2`,
		f.Id, f.ProjectUUID, f.Title, f.Description, []byte(f.Fields), f.TitleField, f.Priority, f.AssigneeUUID, f.Active, f.CreatedBy)
	if err != nil {
		return nil, err
	}
	return Get(f.ProjectUUID, f.Id)
}

// Delete removes a form; tasks it made stay.
func Delete(project, id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM project_forms WHERE id = $1 AND project_uuid = $2`, id, project)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Record keeps a submission and the task it made.
func Record(form uuid.UUID, task *uuid.UUID, answers json.RawMessage) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO form_submissions (form_id, task_uuid, answers) VALUES ($1, $2, $3)`, form, task, []byte(answers))
	return err
}

// CountSince is how many submissions a form took since a moment: the brake
// on a form being flooded.
func CountSince(form uuid.UUID, since time.Time) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM form_submissions WHERE form_id = $1 AND created_at > $2`, form, since).Scan(&n)
	return n, err
}
