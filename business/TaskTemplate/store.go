package business

// Keeping templates (store): a project's, or with no project the
// workspace's, in Postgres (models/postgres/TaskTemplate), each checked
// before it is kept, so what is kept can always be used. The shape is the
// row's body; its name, description and default are columns a list reads.
// Who may keep or use them is the handlers' to ask (MayManage, MayApply):
// nothing here checks it.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/TaskTemplate"
	"github.com/google/uuid"
)

// ErrNotFound: no such template, or none kept where it was asked for.
var ErrNotFound = errors.New("no such template")

// Summary is a template as a list shows it, without its shape.
type Summary struct {
	ID string `json:"id"`
	// ProjectID is "" for the workspace's.
	ProjectID   string    `json:"project_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	DefaultFor  string    `json:"default_for,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Seams for tests: the model.
var (
	listRows      = model.List
	workspaceRows = model.ListWorkspace
	getRow        = model.Get
	addRow        = model.AddWithin
	addRows       = model.AddAllWithin
	updateRow     = model.Update
	deleteRow     = model.Delete
	restoreRow    = model.RestoreWithin
)

// idOf is an id the model keeps as a string, "" for none.
func idOf(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// idIn is a checked template's id as the model keeps it, nil for "".
func idIn(s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func summaries(rows []model.Template, err error) ([]Summary, error) {
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(rows))
	for _, r := range rows {
		out = append(out, Summary{ID: r.ID.String(), ProjectID: idOf(r.ProjectID), Name: r.Name, Description: r.Description,
			DefaultFor: idOf(r.DefaultFor), UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// List is the templates a task in project can start from: the project's,
// then the workspace's, each by name.
func List(ctx context.Context, project uuid.UUID) ([]Summary, error) {
	return summaries(listRows(ctx, project))
}

// ListWorkspace is the workspace's templates by name.
func ListWorkspace(ctx context.Context) ([]Summary, error) {
	return summaries(workspaceRows(ctx))
}

// Get is a template in full, and whether it is deleted. ErrNotFound when
// there's none.
func Get(ctx context.Context, id string) (Template, bool, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return Template{}, false, ErrNotFound
	}
	r, err := getRow(ctx, uid)
	if err != nil {
		return Template{}, false, err
	}
	if r == nil {
		return Template{}, false, ErrNotFound
	}
	t := Template{ID: r.ID.String(), ProjectID: idOf(r.ProjectID), Name: r.Name, Description: r.Description, DefaultFor: idOf(r.DefaultFor)}
	if err := json.Unmarshal(r.Body, &t.Shape); err != nil {
		return Template{}, false, err
	}
	return t, r.DeletedAt != nil, nil
}

// Usable says whether a task in project may start from t: the project's own
// templates and the workspace's, never another project's. Pure.
func Usable(t Template, project uuid.UUID) bool {
	return t.ProjectID == "" || t.ProjectID == project.String()
}

// rowOf is a checked template as the model keeps it: its shape the body.
func rowOf(t Template) (*model.Template, error) {
	body, err := json.Marshal(t.Shape)
	if err != nil {
		return nil, err
	}
	r := &model.Template{Name: t.Name, Description: t.Description, Body: body}
	if r.ProjectID, err = idIn(t.ProjectID); err != nil {
		return nil, err
	}
	if r.DefaultFor, err = idIn(t.DefaultFor); err != nil {
		return nil, err
	}
	return r, nil
}

// refusal is the model's refusal of t in words.
func refusal(err error, t Template) error {
	switch {
	case errors.Is(err, model.ErrNameTaken):
		return fix("A template named %q is already kept here. Pick another name.", t.Name)
	case errors.Is(err, model.ErrDefaultTaken):
		return fix("Another template here is already the default for that type. Make it not the default first.")
	case errors.Is(err, model.ErrNotFound):
		return ErrNotFound
	}
	return err
}

// Save keeps a new template where t.ProjectID says ("" for the workspace),
// as by, while that scope keeps fewer than MaxKept, and answers it as kept.
func Save(ctx context.Context, t Template, by uuid.UUID) (Template, error) {
	t.ID = ""
	t, err := Check(t)
	if err != nil {
		return Template{}, err
	}
	r, err := rowOf(t)
	if err != nil {
		return Template{}, err
	}
	r.CreatedBy = by
	added, err := addRow(ctx, r, MaxKept)
	if err != nil {
		return Template{}, refusal(err, t)
	}
	if !added {
		return Template{}, full(r.ProjectID, " first")
	}
	t.ID = r.ID.String()
	return t, nil
}

// SaveAll keeps new templates, all in project's scope ("" for the
// workspace's), as by, in one write while that scope keeps no more than
// MaxKept with them, and answers them as kept, in order. Each is checked
// first; one that fails, or a name or default already taken, keeps none, so
// a seed or an import keeps its set whole.
func SaveAll(ctx context.Context, project string, ts []Template, by uuid.UUID) ([]Template, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	out := make([]Template, len(ts))
	rows := make([]*model.Template, len(ts))
	for i, t := range ts {
		t.ID, t.ProjectID = "", project
		checked, err := Check(t)
		if err != nil {
			return nil, err
		}
		r, err := rowOf(checked)
		if err != nil {
			return nil, err
		}
		r.CreatedBy = by
		out[i], rows[i] = checked, r
	}
	scope := rows[0].ProjectID
	added, err := addRows(ctx, scope, rows, MaxKept)
	switch {
	case errors.Is(err, model.ErrNameTaken):
		return nil, fix("A template with one of those names is already kept here. Pick other names.")
	case err != nil:
		return nil, refusal(err, Template{})
	case !added:
		return nil, full(scope, " first")
	}
	for i, r := range rows {
		out[i].ID = r.ID.String()
	}
	return out, nil
}

// Update changes a live template's name, description, default and shape,
// as by. Where it is kept never changes.
func Update(ctx context.Context, t Template, by uuid.UUID) (Template, error) {
	id, err := uuid.Parse(t.ID)
	if err != nil {
		return Template{}, ErrNotFound
	}
	if t, err = Check(t); err != nil {
		return Template{}, err
	}
	r, err := rowOf(t)
	if err != nil {
		return Template{}, err
	}
	r.ID = id
	if err := updateRow(ctx, r, by); err != nil {
		return Template{}, refusal(err, t)
	}
	t.ID = id.String()
	return t, nil
}

// full refuses a template its scope has no room for: the project's, or with
// nil the workspace's, which keeps MaxKept; then is what to do after deleting
// one.
func full(project *uuid.UUID, then string) error {
	where := "project"
	if project == nil {
		where = "workspace"
	}
	return fix("This %s keeps %d templates, the most it can. Delete one it no longer uses%s.", where, MaxKept, then)
}

// Delete deletes a live template softly, as by; Restore undoes it.
func Delete(ctx context.Context, id string, by uuid.UUID) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return ErrNotFound
	}
	return refusal(deleteRow(ctx, uid, by), Template{})
}

// Restore brings a deleted template back, unless a live one has taken its
// name or its default meanwhile.
func Restore(ctx context.Context, t Template) error {
	uid, err := uuid.Parse(t.ID)
	if err != nil {
		return ErrNotFound
	}
	var project *uuid.UUID
	if t.ProjectID != "" {
		p, err := uuid.Parse(t.ProjectID)
		if err != nil {
			return ErrNotFound
		}
		project = &p
	}
	restored, err := restoreRow(ctx, uid, project, MaxKept)
	switch {
	case errors.Is(err, model.ErrNameTaken):
		return fix("Another template here is named %q now. Rename one of them, then restore this one.", t.Name)
	case errors.Is(err, model.ErrDefaultTaken):
		return fix("Another template here is the default for that type now. Make it not the default, then restore this one.")
	case err != nil:
		return refusal(err, t)
	case !restored:
		return full(project, ", then restore this one")
	}
	return nil
}
