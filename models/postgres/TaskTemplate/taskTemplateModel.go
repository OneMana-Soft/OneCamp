// Package models (TaskTemplate) stores task templates (migration 554): a
// project's, or with no project the workspace's. The template itself is
// business/TaskTemplate's to read and check; here it is a JSON body beside
// what a list shows. Deleting one is soft, and a deleted one's name and
// default are free while it stays deleted.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

var (
	// ErrNameTaken: another live template of the same project, or of the
	// workspace, has that name, in any case.
	ErrNameTaken = errors.New("a template with that name already exists here")
	// ErrDefaultTaken: another live template of the same project, or of the
	// workspace, is the default for that type.
	ErrDefaultTaken = errors.New("another template is already the default for that type here")
	// ErrNotFound: no live template has that id (or, to restore, no deleted one).
	ErrNotFound = errors.New("no such template")
)

// Template is one kept template. Body is left out of a list.
type Template struct {
	ID          uuid.UUID
	ProjectID   *uuid.UUID // nil: the workspace's
	Name        string
	Description string
	Body        []byte
	DefaultFor  *uuid.UUID
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	UpdatedBy   *uuid.UUID
	UpdatedAt   time.Time
	DeletedAt   *time.Time
	DeletedBy   *uuid.UUID
}

// workspace stands for "no project" in the scope the unique indexes and a
// project's list read (migration 554).
const workspace = "00000000-0000-0000-0000-000000000000"

const columns = `id, project_id, name, description, default_for, created_by, created_at, updated_by, updated_at, deleted_at, deleted_by`

// taken is a unique index's refusal in words: the default's, or the name's.
func taken(err error) error {
	switch {
	case helpers.IsUniqueViolationOn(err, "task_templates_default_live"):
		return ErrDefaultTaken
	case helpers.IsUniqueViolation(err):
		return ErrNameTaken
	}
	return err
}

func scan(row pgexec.Row, body bool) (Template, error) {
	var t Template
	dest := []any{&t.ID, &t.ProjectID, &t.Name, &t.Description, &t.DefaultFor, &t.CreatedBy, &t.CreatedAt,
		&t.UpdatedBy, &t.UpdatedAt, &t.DeletedAt, &t.DeletedBy}
	if body {
		dest = append(dest, &t.Body)
	}
	return t, row.Scan(dest...)
}

func summary(row pgexec.Row) (Template, error) { return scan(row, false) }

// Add keeps a new template and fills in its id and times.
func Add(ctx context.Context, t *Template) error {
	_, _, err := pgexec.QueryOne(ctx, `
		INSERT INTO task_templates (project_id, name, description, body, default_for, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at, updated_at`,
		func(r pgexec.Row) (bool, error) { return true, r.Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt) },
		t.ProjectID, t.Name, t.Description, t.Body, t.DefaultFor, t.CreatedBy)
	return taken(err)
}

// AddWithin is Add while t's project, or the workspace, keeps fewer than
// limit live templates: the count and the insert run in one transaction
// under the scope's lock (pgexec.Capped), so two at once can't both take the
// last place. added is false when the scope is full.
func AddWithin(ctx context.Context, t *Template, limit int) (added bool, err error) {
	return pgexec.Capped(ctx, capKey(t.ProjectID), limit,
		func(ctx context.Context) (int, error) { return Count(ctx, t.ProjectID) },
		func(ctx context.Context) error { return Add(ctx, t) })
}

// AddAllWithin keeps rows, every one in project's scope (nil: the
// workspace's), in one statement while the scope keeps no more than limit
// live templates with them, and fills in each one's id and times. The count
// and the insert hold the scope's lock as AddWithin's do. added is false, and
// none is kept, when they don't all fit; a name or default already taken
// keeps none either.
func AddAllWithin(ctx context.Context, project *uuid.UUID, rows []*Template, limit int) (added bool, err error) {
	if len(rows) == 0 {
		return true, nil
	}
	// Capped adds while fewer than its limit are kept, so there is room for
	// all of rows while fewer than limit-len(rows)+1 are.
	return pgexec.Capped(ctx, capKey(project), limit-len(rows)+1,
		func(ctx context.Context) (int, error) { return Count(ctx, project) },
		func(ctx context.Context) error { return addAll(ctx, project, rows) })
}

// addAll keeps rows in project's scope in one INSERT and fills in their ids
// and times, matched by name: a scope's live templates' names all differ.
func addAll(ctx context.Context, project *uuid.UUID, rows []*Template) error {
	n := len(rows)
	names, about, bodies, defaults, by := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	byName := make(map[string]*Template, n)
	for i, r := range rows {
		names[i], about[i], bodies[i], by[i] = r.Name, r.Description, string(r.Body), r.CreatedBy.String()
		if r.DefaultFor != nil {
			defaults[i] = r.DefaultFor.String()
		}
		byName[r.Name] = r
	}
	_, err := pgexec.Query(ctx, `
		INSERT INTO task_templates (project_id, name, description, body, default_for, created_by)
		SELECT $1::uuid, t.name, t.description, t.body::jsonb, NULLIF(t.default_for, '')::uuid, t.created_by::uuid
		  FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[]) AS t(name, description, body, default_for, created_by)
		RETURNING id, name, created_at, updated_at`,
		func(r pgexec.Row) (bool, error) {
			var id uuid.UUID
			var name string
			var created, updated time.Time
			if err := r.Scan(&id, &name, &created, &updated); err != nil {
				return false, err
			}
			if t := byName[name]; t != nil {
				t.ID, t.CreatedAt, t.UpdatedAt = id, created, updated
			}
			return true, nil
		}, project, pq.Array(names), pq.Array(about), pq.Array(bodies), pq.Array(defaults), pq.Array(by))
	return taken(err)
}

// Update changes a live template's name, description, body and default, as
// by, and fills in its times.
func Update(ctx context.Context, t *Template, by uuid.UUID) error {
	// No body keeps the one kept: a nil slice goes as SQL NULL.
	var body any
	if t.Body != nil {
		body = t.Body
	}
	_, found, err := pgexec.QueryOne(ctx, `
		UPDATE task_templates SET name = $2, description = $3, body = COALESCE($4, body), default_for = $5,
		       updated_by = $6, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL RETURNING updated_at`,
		func(r pgexec.Row) (bool, error) { return true, r.Scan(&t.UpdatedAt) },
		t.ID, t.Name, t.Description, body, t.DefaultFor, by)
	if err != nil {
		return taken(err)
	}
	if !found {
		return ErrNotFound
	}
	t.UpdatedBy = &by
	return nil
}

// list is the rows a query reads, never nil.
func list(ctx context.Context, query string, args ...any) ([]Template, error) {
	out, err := pgexec.Query(ctx, query, summary, args...)
	if out == nil && err == nil {
		out = []Template{}
	}
	return out, err
}

// List is the live templates a task in project can start from, without
// bodies: the project's, then the workspace's, each by name.
func List(ctx context.Context, project uuid.UUID) ([]Template, error) {
	return list(ctx, `SELECT `+columns+` FROM task_templates
		 WHERE deleted_at IS NULL AND COALESCE(project_id, '`+workspace+`'::uuid) IN ($1, '`+workspace+`'::uuid)
		 ORDER BY project_id IS NULL, lower(name)`, project)
}

// ListWorkspace is the workspace's live templates by name, without bodies.
func ListWorkspace(ctx context.Context) ([]Template, error) {
	return list(ctx, `SELECT `+columns+` FROM task_templates
		 WHERE deleted_at IS NULL AND COALESCE(project_id, '`+workspace+`'::uuid) = '`+workspace+`'::uuid
		 ORDER BY lower(name)`)
}

// Get is a template with its body, live or deleted; nil when there's none.
func Get(ctx context.Context, id uuid.UUID) (*Template, error) {
	t, found, err := pgexec.QueryOne(ctx, `SELECT `+columns+`, body FROM task_templates WHERE id = $1`,
		func(r pgexec.Row) (Template, error) { return scan(r, true) }, id)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// Count is how many live templates a project keeps, or with nil the workspace.
func Count(ctx context.Context, project *uuid.UUID) (int, error) {
	scope := uuid.MustParse(workspace)
	if project != nil {
		scope = *project
	}
	return pgexec.Scalar[int](ctx, `SELECT count(*) FROM task_templates
		 WHERE deleted_at IS NULL AND COALESCE(project_id, '`+workspace+`'::uuid) = $1`, scope)
}

// Delete deletes a live template softly, as by.
func Delete(ctx context.Context, id, by uuid.UUID) error {
	return changeOne(ctx, `UPDATE task_templates SET deleted_at = NOW(), deleted_by = $2
		 WHERE id = $1 AND deleted_at IS NULL`, id, by)
}

// restore brings a deleted template back, unless a live one has taken its
// name or its default meanwhile (RestoreWithin keeps the cap).
func restore(ctx context.Context, id uuid.UUID) error {
	return changeOne(ctx, `UPDATE task_templates SET deleted_at = NULL, deleted_by = NULL
		 WHERE id = $1 AND deleted_at IS NOT NULL`, id)
}

// RestoreWithin is Restore while the template's project, or with nil the
// workspace, keeps fewer than limit live templates: the count and the
// restore run under the scope's lock, as AddWithin's count and insert do, so
// a restore can't take a place an add is taking. restored is false when the
// scope is full.
func RestoreWithin(ctx context.Context, id uuid.UUID, project *uuid.UUID, limit int) (restored bool, err error) {
	return pgexec.Capped(ctx, capKey(project), limit,
		func(ctx context.Context) (int, error) { return Count(ctx, project) },
		func(ctx context.Context) error { return restore(ctx, id) })
}

// capKey names the lock a scope's cap is kept under: a project's, or with nil
// the workspace's.
func capKey(project *uuid.UUID) string {
	if project == nil {
		return "task_templates:" + workspace
	}
	return "task_templates:" + project.String()
}

func changeOne(ctx context.Context, query string, args ...any) error {
	n, err := pgexec.Exec(ctx, query, args...)
	if err != nil {
		return taken(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
