package business

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/ProjectTemplate"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Summary is a template as the picker lists it.
type Summary struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	BuiltIn     bool       `json:"built_in"`
	TaskCount   int        `json:"task_count"`
	Preview     []string   `json:"preview"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	CanDelete   bool       `json:"can_delete"`
}

func summaryOf(t Template) Summary {
	return Summary{ID: t.ID, Name: t.Name, Description: t.Description, BuiltIn: builtIn(t.ID) != nil, TaskCount: t.Size(), Preview: t.Preview()}
}

// savedSummary is a saved template as the picker lists it, for the reader me.
func savedSummary(r *model.Template, author string, me *userModels.UserInfo) Summary {
	return Summary{
		ID: r.ID.String(), Name: r.Name, Description: r.Description, TaskCount: r.TaskCount, Preview: r.Preview,
		CreatedBy: author, CreatedAt: &r.CreatedAt, CanDelete: mayDelete(r, me),
	}
}

// mayDelete: a saved template is its author's, and any admin's, to delete.
func mayDelete(row *model.Template, me *userModels.UserInfo) bool {
	return me.UserPostgresInfo.IsAdmin || row.CreatedBy == me.UserPostgresInfo.Id
}

// List is the built-in templates, then the saved ones, newest first.
func List(ctx context.Context, me *userModels.UserInfo) ([]Summary, error) {
	rows, err := model.List(ctx, MaxSaved)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(builtins)+len(rows))
	for _, t := range builtins {
		out = append(out, summaryOf(t))
	}
	authors := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		authors = append(authors, r.CreatedBy)
	}
	names, err := userModels.DisplayNamesByUUIDs(ctx, authors)
	if err != nil {
		names = nil // the list still shows, without who saved each
	}
	for i := range rows {
		out = append(out, savedSummary(&rows[i], names[rows[i].CreatedBy], me))
	}
	return out, nil
}

// Get is a template in full: a built-in one by its slug, a saved one by its
// id. ErrNotFound when there is no such template.
func Get(ctx context.Context, id string) (Template, error) {
	if t := builtIn(id); t != nil {
		return *t, nil
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return Template{}, ErrNotFound
	}
	row, err := model.Get(ctx, uid)
	if err != nil {
		return Template{}, err
	}
	if row == nil {
		return Template{}, ErrNotFound
	}
	var t Template
	if err := json.Unmarshal(row.Body, &t); err != nil {
		return Template{}, err
	}
	t.ID, t.Name, t.Description = row.ID.String(), row.Name, row.Description
	return t, nil
}

// Save keeps a template for everyone who creates projects; author is who
// saved it. The template is checked first, so what's kept can always be used.
func Save(ctx context.Context, t Template, author *userModels.UserInfo) (Summary, error) {
	t, err := Check(t)
	if err != nil {
		return Summary{}, err
	}
	if n, err := model.Count(ctx); err != nil {
		return Summary{}, err
	} else if n >= MaxSaved {
		return Summary{}, fix("This workspace has %d saved templates, the most it keeps. Delete one you no longer use first.", n)
	}
	body, err := json.Marshal(Template{Statuses: t.Statuses, Tasks: t.Tasks})
	if err != nil {
		return Summary{}, err
	}
	row := &model.Template{Name: t.Name, Description: t.Description, Body: body, TaskCount: t.Size(), Preview: t.Preview(), CreatedBy: author.UserPostgresInfo.Id}
	if err := model.Add(ctx, row); err != nil {
		if errors.Is(err, model.ErrNameTaken) {
			return Summary{}, fix("A template named %q already exists. Pick another name.", t.Name)
		}
		return Summary{}, err
	}
	names, _ := userModels.DisplayNamesByUUIDs(ctx, []uuid.UUID{row.CreatedBy})
	return savedSummary(row, names[row.CreatedBy], author), nil
}

// Delete removes a saved template. Built-in ones can't be deleted.
func Delete(ctx context.Context, id string, me *userModels.UserInfo) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return ErrNotFound
	}
	row, err := model.Get(ctx, uid)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if !mayDelete(row, me) {
		return ErrNotYours
	}
	if gone, err := model.Delete(ctx, uid); err != nil {
		return err
	} else if !gone {
		return ErrNotFound
	}
	return nil
}
