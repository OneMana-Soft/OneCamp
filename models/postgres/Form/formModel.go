package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Form is a project's intake form (migrations 179, 493 and 494), or a
// table's own form (930): one whose home is the table it adds rows to, with
// no project (ProjectUUID uuid.Nil, NULL in the store).
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
	// Confirmation is what a visitor reads once the form is sent ("" for the
	// app's own words).
	Confirmation string `json:"confirmation"`
	// TargetTable is the data table the form adds a row to; nil for a form
	// that makes tasks.
	TargetTable *uuid.UUID `json:"target_table,omitempty"`
	// RedirectURL is the page a visitor goes to once it's sent (931); "" for
	// its message.
	RedirectURL string `json:"redirect"`
	// CoverID and LogoID are the attachments its managers chose as its cover
	// and its logo (938, tables B5c-2); nil for none.
	CoverID     *uuid.UUID `json:"cover_id,omitempty"`
	LogoID      *uuid.UUID `json:"logo_id,omitempty"`
	CreatedBy   uuid.UUID  `json:"created_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Submissions int        `json:"submissions"`
}

// Submission is one time a form was sent: its answers, and the task it made
// or the table row it added.
type Submission struct {
	Id        uuid.UUID       `json:"id"`
	TaskUUID  *uuid.UUID      `json:"task_uuid,omitempty"`
	RowID     *uuid.UUID      `json:"row_id,omitempty"`
	Answers   json.RawMessage `json:"answers"`
	CreatedAt time.Time       `json:"created_at"`
}

// A table's own form has no project: it reads as uuid.Nil.
const columns = `f.id, COALESCE(f.project_uuid, '00000000-0000-0000-0000-000000000000'::uuid), f.token, f.title, f.description, f.fields, f.title_field, f.priority,
	f.assignee_uuid, f.active, f.confirmation, f.target_table, f.redirect_url, f.cover_id, f.logo_id, f.created_by, f.updated_at,
	(SELECT COUNT(*) FROM form_submissions s WHERE s.form_id = f.id)`

func scan(row pgexec.Row) (*Form, error) {
	var f Form
	err := row.Scan(&f.Id, &f.ProjectUUID, &f.Token, &f.Title, &f.Description, &f.Fields, &f.TitleField, &f.Priority,
		&f.AssigneeUUID, &f.Active, &f.Confirmation, &f.TargetTable, &f.RedirectURL, &f.CoverID, &f.LogoID, &f.CreatedBy, &f.UpdatedAt, &f.Submissions)
	return &f, err
}

// one is the form a statement reads, or nil.
func one(ctx context.Context, q string, args ...any) (*Form, error) {
	f, found, err := pgexec.QueryOne(ctx, q, scan, args...)
	if err != nil || !found {
		return nil, err
	}
	return f, nil
}

// List is a project's forms, newest first.
func List(ctx context.Context, project uuid.UUID) ([]Form, error) {
	forms, err := pgexec.Query(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.project_uuid = $1 AND `+softDelete.Live("f")+` ORDER BY f.created_at DESC`,
		func(r pgexec.Row) (Form, error) {
			f, err := scan(r)
			if err != nil {
				return Form{}, err
			}
			return *f, nil
		}, project)
	if forms == nil && err == nil {
		forms = []Form{}
	}
	return forms, err
}

// Count is how many forms a project has.
func Count(ctx context.Context, project uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM project_forms WHERE project_uuid = $1 AND `+softDelete.Live(""), project)
}

// MakesTasks says whether a form is the project's and makes tasks (adds no
// table rows): one a project rule can run on.
func MakesTasks(ctx context.Context, project, id uuid.UUID) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM project_forms WHERE id = $1 AND project_uuid = $2 AND target_table IS NULL AND `+softDelete.Live("")+`)`, id, project)
}

// ByToken is the form a public link points at, or nil.
func ByToken(ctx context.Context, token string) (*Form, error) {
	return one(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.token = $1 AND `+softDelete.Live("f"), token)
}

// Get is a form of the project, or nil.
func Get(ctx context.Context, project, id uuid.UUID) (*Form, error) {
	return one(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.id = $1 AND f.project_uuid = $2 AND `+softDelete.Live("f"), id, project)
}

// Create stores a new form.
func Create(ctx context.Context, f Form) (*Form, error) {
	id, err := pgexec.Scalar[uuid.UUID](ctx, `
		INSERT INTO project_forms (project_uuid, token, title, description, fields, title_field, priority, assignee_uuid, active, confirmation, target_table, created_by, redirect_url, cover_id, logo_id)
		VALUES (NULLIF($1::uuid, '00000000-0000-0000-0000-000000000000'::uuid), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
		f.ProjectUUID, f.Token, f.Title, f.Description, []byte(f.Fields), f.TitleField, f.Priority, f.AssigneeUUID, f.Active, f.Confirmation, f.TargetTable, f.CreatedBy, f.RedirectURL, f.CoverID, f.LogoID)
	if err != nil {
		return nil, err
	}
	if f.ProjectUUID == uuid.Nil {
		return GetOfTable(ctx, *f.TargetTable, id)
	}
	return Get(ctx, f.ProjectUUID, id)
}

// CreateWithin stores a new form, as Create does, while where it lives has
// fewer than limit forms: its project, or for a table's own form (no project)
// its table. When there's no room it stores nothing and answers a nil form
// with the count it found. The count and the insert hold one transaction lock
// on that project or table (pgexec.Capped): counted apart from the insert,
// two forms made at once could each see room for the last place.
func CreateWithin(ctx context.Context, f Form, limit int) (*Form, int, error) {
	key, count := "project_forms:"+f.ProjectUUID.String(), func(ctx context.Context) (int, error) { return Count(ctx, f.ProjectUUID) }
	if f.ProjectUUID == uuid.Nil {
		if f.TargetTable == nil {
			return nil, 0, errNoHome
		}
		table := *f.TargetTable
		key, count = "table_forms:"+table.String(), func(ctx context.Context) (int, error) { return CountOfTable(ctx, table) }
	}
	var made *Form
	found := 0
	_, err := pgexec.Capped(ctx, key, limit,
		func(ctx context.Context) (int, error) {
			n, err := count(ctx)
			found = n
			return n, err
		},
		func(ctx context.Context) error {
			var err error
			made, err = Create(ctx, f)
			return err
		})
	if err != nil {
		return nil, found, err
	}
	return made, found, nil
}

// errNoHome is a form with neither a project nor a table, which the store's
// own check refuses too (525_table_forms).
var errNoHome = errors.New("form: a form needs a project or a table")

// Update changes a form's content; its link stays.
func Update(ctx context.Context, f Form) (*Form, error) {
	if _, err := pgexec.Exec(ctx, `
		UPDATE project_forms SET title = $3, description = $4, fields = $5, title_field = $6, priority = $7,
		       assignee_uuid = $8, active = $9, confirmation = $10, target_table = $11, created_by = $12, redirect_url = $13, cover_id = $14, logo_id = $15, updated_at = NOW()
		 WHERE id = $1 AND project_uuid = $2 AND `+softDelete.Live(""),
		f.Id, f.ProjectUUID, f.Title, f.Description, []byte(f.Fields), f.TitleField, f.Priority, f.AssigneeUUID, f.Active, f.Confirmation, f.TargetTable, f.CreatedBy, f.RedirectURL, f.CoverID, f.LogoID); err != nil {
		return nil, err
	}
	return Get(ctx, f.ProjectUUID, f.Id)
}

// Delete deletes a project's form softly (V-78): its link stops taking
// answers, and its submissions and the tasks it made stay for its restore.
func Delete(ctx context.Context, project, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.Form, id, by, "project_uuid = $3", project)
}

// Record keeps a submission and the task it made or the row it added, and
// answers its id.
func Record(ctx context.Context, form uuid.UUID, task, row *uuid.UUID, answers json.RawMessage) (uuid.UUID, error) {
	return pgexec.Scalar[uuid.UUID](ctx,
		`INSERT INTO form_submissions (form_id, task_uuid, row_id, answers) VALUES ($1, $2, $3, $4) RETURNING id`, form, task, row, []byte(answers))
}

// Latest is a form's newest submissions, at most n.
func Latest(ctx context.Context, form uuid.UUID, n int) ([]Submission, error) {
	out, err := pgexec.Query(ctx, `SELECT id, task_uuid, row_id, answers, created_at FROM form_submissions WHERE form_id = $1 ORDER BY created_at DESC LIMIT $2`,
		func(r pgexec.Row) (Submission, error) {
			var s Submission
			err := r.Scan(&s.Id, &s.TaskUUID, &s.RowID, &s.Answers, &s.CreatedAt)
			return s, err
		}, form, n)
	if out == nil && err == nil {
		out = []Submission{}
	}
	return out, err
}

// CountSince is how many submissions a form took since a moment: the brake
// on a form being flooded.
func CountSince(ctx context.Context, form uuid.UUID, since time.Time) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM form_submissions WHERE form_id = $1 AND created_at > $2`, form, since)
}

// ProjectsOf is each form's project, by the form's id; a form that isn't
// there, or is a table's own (no project), is left out.
func ProjectsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]uuid.UUID{}, nil
	}
	return pgexec.Pairs[uuid.UUID, uuid.UUID](ctx, `SELECT id, project_uuid FROM project_forms WHERE id = ANY($1) AND project_uuid IS NOT NULL AND `+softDelete.Live(""), ids)
}

// ── A table's own forms (930): their home is the table they fill. ──

// ListOfTable is a table's own forms, newest first.
func ListOfTable(ctx context.Context, table uuid.UUID) ([]Form, error) {
	forms, err := pgexec.Query(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.target_table = $1 AND f.project_uuid IS NULL AND `+softDelete.Live("f")+` ORDER BY f.created_at DESC`,
		func(r pgexec.Row) (Form, error) {
			f, err := scan(r)
			if err != nil {
				return Form{}, err
			}
			return *f, nil
		}, table)
	if forms == nil && err == nil {
		forms = []Form{}
	}
	return forms, err
}

// CountOfTable is how many forms of its own a table has.
func CountOfTable(ctx context.Context, table uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM project_forms WHERE target_table = $1 AND project_uuid IS NULL AND `+softDelete.Live(""), table)
}

// GetOfTable is one of a table's own forms, or nil.
func GetOfTable(ctx context.Context, table, id uuid.UUID) (*Form, error) {
	return one(ctx, `SELECT `+columns+` FROM project_forms f WHERE f.id = $1 AND f.target_table = $2 AND f.project_uuid IS NULL AND `+softDelete.Live("f"), id, table)
}

// UpdateOfTable saves one of a table's own forms; nil when the table has no
// such form.
func UpdateOfTable(ctx context.Context, table uuid.UUID, f Form) (*Form, error) {
	if _, err := pgexec.Exec(ctx, `
		UPDATE project_forms SET title = $3, description = $4, fields = $5, active = $6, confirmation = $7, created_by = $8, redirect_url = $9, cover_id = $10, logo_id = $11, updated_at = NOW()
		 WHERE id = $1 AND target_table = $2 AND project_uuid IS NULL AND `+softDelete.Live(""),
		f.Id, table, f.Title, f.Description, []byte(f.Fields), f.Active, f.Confirmation, f.CreatedBy, f.RedirectURL, f.CoverID, f.LogoID); err != nil {
		return nil, err
	}
	return GetOfTable(ctx, table, f.Id)
}

// DeleteOfTable deletes one of a table's own forms softly (V-78), as Delete
// does a project's.
func DeleteOfTable(ctx context.Context, table, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.Form, id, by, "target_table = $3 AND project_uuid IS NULL", table)
}
