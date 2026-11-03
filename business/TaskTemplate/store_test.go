package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	model "github.com/akashc777/OneCamp/models/postgres/TaskTemplate"
	"github.com/google/uuid"
)

// keptRows stands in for the model: n live templates in each scope, and the
// row each write was given. add, change, del and restore answer err.
type keptRows struct {
	n                         int
	added, changed            *model.Template
	limit                     int
	called                    bool
	by                        uuid.UUID
	addErr, changeErr, delErr error
	restoreErr                error
	scope                     *uuid.UUID
	row                       *model.Template
	rows                      []model.Template
}

func stubRows(t *testing.T) *keptRows {
	saved := struct {
		l  func(context.Context, uuid.UUID) ([]model.Template, error)
		lw func(context.Context) ([]model.Template, error)
		g  func(context.Context, uuid.UUID) (*model.Template, error)
		a  func(context.Context, *model.Template, int) (bool, error)
		u  func(context.Context, *model.Template, uuid.UUID) error
		d  func(context.Context, uuid.UUID, uuid.UUID) error
		r  func(context.Context, uuid.UUID, *uuid.UUID, int) (bool, error)
	}{listRows, workspaceRows, getRow, addRow, updateRow, deleteRow, restoreRow}
	t.Cleanup(func() {
		listRows, workspaceRows, getRow, addRow, updateRow, deleteRow, restoreRow = saved.l, saved.lw, saved.g, saved.a, saved.u, saved.d, saved.r
	})
	k := &keptRows{}
	listRows = func(context.Context, uuid.UUID) ([]model.Template, error) { return k.rows, nil }
	workspaceRows = func(context.Context) ([]model.Template, error) { return k.rows, nil }
	getRow = func(context.Context, uuid.UUID) (*model.Template, error) { return k.row, nil }
	addRow = func(_ context.Context, r *model.Template, limit int) (bool, error) {
		k.called, k.limit = true, limit
		if k.addErr != nil || k.n >= limit {
			return false, k.addErr
		}
		r.ID, k.added = uuid.New(), r
		return true, nil
	}
	updateRow = func(_ context.Context, r *model.Template, by uuid.UUID) error {
		k.changed, k.by = r, by
		return k.changeErr
	}
	deleteRow = func(_ context.Context, _, by uuid.UUID) error { k.by = by; return k.delErr }
	restoreRow = func(_ context.Context, _ uuid.UUID, scope *uuid.UUID, limit int) (bool, error) {
		k.scope, k.limit = scope, limit
		if k.restoreErr != nil || k.n >= limit {
			return false, k.restoreErr
		}
		return true, nil
	}
	return k
}

func wantFix(t *testing.T, what string, err error, words ...string) {
	t.Helper()
	var te *TemplateError
	if !errors.As(err, &te) {
		t.Fatalf("%s: %v, want a TemplateError", what, err)
	}
	for _, w := range words {
		if !strings.Contains(te.Error(), w) {
			t.Errorf("%s: %q doesn't say %q", what, te.Error(), w)
		}
	}
}

func TestSaveKeepsATemplateWhereItBelongs(t *testing.T) {
	k := stubRows(t)
	project, bug, by := uuid.New(), uuid.New(), uuid.New()
	two := 2
	in := Template{ID: "ignored", ProjectID: project.String(), Name: "  Bug report ", Description: "For bugs", DefaultFor: bug.String(),
		Shape: Shape{Task: projectTemplate.Task{Name: "Bug: ", Priority: "high", DueDay: &two, Subtasks: []projectTemplate.Subtask{{Name: "Reproduce"}}},
			Required: []string{"due"}}}
	got, err := Save(context.Background(), in, by)
	if err != nil {
		t.Fatal(err)
	}
	r := k.added
	if r == nil || r.ProjectID == nil || *r.ProjectID != project || k.limit != MaxKept {
		t.Fatalf("kept in the wrong scope: %+v (under %d)", r, k.limit)
	}
	if r.Name != "Bug report" || r.Description != "For bugs" || r.DefaultFor == nil || *r.DefaultFor != bug || r.CreatedBy != by {
		t.Errorf("the row: %+v", r)
	}
	// The task's name is kept as typed: the maker types after "Bug: ".
	var body Shape
	if err := json.Unmarshal(r.Body, &body); err != nil || body.Name != "Bug: " || body.Type != bug.String() || len(body.Subtasks) != 1 ||
		body.DueDay == nil || *body.DueDay != 2 || len(body.Required) != 1 {
		t.Errorf("the body: %s (%v)", r.Body, err)
	}
	if got.ID != r.ID.String() || got.Name != "Bug report" || got.Shape.Type != bug.String() {
		t.Errorf("answered %+v", got)
	}

	// The workspace's is kept with no project.
	if _, err := Save(context.Background(), Template{Name: "Request"}, by); err != nil || k.added.ProjectID != nil {
		t.Errorf("a workspace template: %v, %+v", err, k.added)
	}
}

func TestSaveSaysWhyItCant(t *testing.T) {
	k := stubRows(t)
	ctx, by := context.Background(), uuid.New()
	_, err := Save(ctx, Template{Name: " "}, by)
	wantFix(t, "no name", err, "name")
	if k.called || k.added != nil {
		t.Error("a template that fails its check reached the store")
	}
	k.n = MaxKept
	_, err = Save(ctx, Template{ProjectID: uuid.NewString(), Name: "One more"}, by)
	wantFix(t, "a full project", err, "100", "project")
	_, err = Save(ctx, Template{Name: "One more"}, by)
	wantFix(t, "a full workspace", err, "100", "workspace")
	k.n = 0
	k.addErr = model.ErrNameTaken
	_, err = Save(ctx, Template{Name: "Bug report"}, by)
	wantFix(t, "a name taken", err, `"Bug report"`)
	k.addErr = model.ErrDefaultTaken
	_, err = Save(ctx, Template{Name: "Bug report", DefaultFor: uuid.NewString()}, by)
	wantFix(t, "a default taken", err, "default")
	k.addErr = errors.New("connection reset")
	if _, err = Save(ctx, Template{Name: "Bug report"}, by); err == nil || errors.As(err, new(*TemplateError)) {
		t.Errorf("a failed write: %v, want the error as it is", err)
	}
}

func TestGetReadsATemplateInFull(t *testing.T) {
	k := stubRows(t)
	ctx := context.Background()
	if _, _, err := Get(ctx, "not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a bad id: %v", err)
	}
	if _, _, err := Get(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Errorf("no row: %v", err)
	}
	project, bug, gone := uuid.New(), uuid.New(), time.Now()
	k.row = &model.Template{ID: uuid.New(), ProjectID: &project, Name: "Bug report", Description: "For bugs", DefaultFor: &bug,
		Body: []byte(`{"name":"Bug: ","type":"` + bug.String() + `","subtasks":[{"name":"Reproduce"}],"required":["due"]}`)}
	got, deleted, err := Get(ctx, k.row.ID.String())
	if err != nil || deleted {
		t.Fatalf("%v, deleted %v", err, deleted)
	}
	if got.ID != k.row.ID.String() || got.ProjectID != project.String() || got.Name != "Bug report" || got.Description != "For bugs" ||
		got.DefaultFor != bug.String() || got.Shape.Name != "Bug: " || len(got.Shape.Subtasks) != 1 || got.Shape.Required[0] != "due" {
		t.Errorf("read %+v", got)
	}
	if !Usable(got, project) || Usable(got, uuid.New()) {
		t.Error("a project's template is usable in that project only")
	}
	k.row.ProjectID, k.row.DeletedAt = nil, &gone
	got, deleted, err = Get(ctx, k.row.ID.String())
	if err != nil || !deleted || got.ProjectID != "" || !Usable(got, uuid.New()) {
		t.Errorf("a deleted workspace template: %+v, deleted %v, %v", got, deleted, err)
	}
}

func TestListShowsWhereEachIsKept(t *testing.T) {
	k := stubRows(t)
	project, bug := uuid.New(), uuid.New()
	k.rows = []model.Template{{ID: uuid.New(), ProjectID: &project, Name: "Bug report", DefaultFor: &bug}, {ID: uuid.New(), Name: "Request"}}
	got, err := List(context.Background(), project)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
	if got[0].ProjectID != project.String() || got[0].DefaultFor != bug.String() || got[1].ProjectID != "" || got[1].Name != "Request" {
		t.Errorf("listed %+v", got)
	}
	k.rows = nil
	if got, err := ListWorkspace(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Errorf("an empty list is [] for the web: %#v, %v", got, err)
	}
}

func TestChangesSayWhyTheyCant(t *testing.T) {
	k := stubRows(t)
	ctx, by, project := context.Background(), uuid.New(), uuid.New()
	id := uuid.NewString()
	got, err := Update(ctx, Template{ID: id, ProjectID: project.String(), Name: " Bug "}, by)
	if err != nil || k.changed == nil || k.changed.ID.String() != id || k.changed.Name != "Bug" || k.by != by || got.ID != id {
		t.Fatalf("an update: %+v, %v, row %+v", got, err, k.changed)
	}
	if _, err := Update(ctx, Template{ID: "x", Name: "Bug"}, by); !errors.Is(err, ErrNotFound) {
		t.Errorf("an update of no id: %v", err)
	}
	k.changeErr = model.ErrNotFound
	if _, err := Update(ctx, Template{ID: id, Name: "Bug"}, by); !errors.Is(err, ErrNotFound) {
		t.Errorf("an update of a deleted one: %v", err)
	}
	k.changeErr = model.ErrNameTaken
	_, err = Update(ctx, Template{ID: id, Name: "Bug"}, by)
	wantFix(t, "an update to a taken name", err, `"Bug"`)
	if err := Delete(ctx, id, by); err != nil || k.by != by {
		t.Errorf("a delete: %v", err)
	}
	k.delErr = model.ErrNotFound
	if err := Delete(ctx, id, by); !errors.Is(err, ErrNotFound) {
		t.Errorf("a delete of a deleted one: %v", err)
	}
	k.restoreErr = model.ErrNameTaken
	wantFix(t, "a restore over a taken name", Restore(ctx, Template{ID: id, Name: "Bug"}), `"Bug"`, "restore")
	k.restoreErr = model.ErrDefaultTaken
	wantFix(t, "a restore over a taken default", Restore(ctx, Template{ID: id, Name: "Bug"}), "default", "restore")
	k.restoreErr = model.ErrNotFound
	if err := Restore(ctx, Template{ID: id}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a restore of a live one: %v", err)
	}
}

// A restore keeps the scope's cap as a save does (CR-167 F3): refused in
// words while the project, or the workspace, keeps MaxKept already.
func TestARestoreKeepsTheCap(t *testing.T) {
	k := stubRows(t)
	ctx := context.Background()
	project := uuid.New()
	k.n = MaxKept
	wantFix(t, "a restore into a full project", Restore(ctx, Template{ID: uuid.NewString(), Name: "Bug", ProjectID: project.String()}), "project", "keeps", "restore")
	if k.limit != MaxKept || k.scope == nil || *k.scope != project {
		t.Errorf("restored within %d in %v, want %d in %s", k.limit, k.scope, MaxKept, project)
	}
	wantFix(t, "a restore into a full workspace", Restore(ctx, Template{ID: uuid.NewString(), Name: "Bug"}), "workspace", "keeps", "restore")
	if k.scope != nil {
		t.Errorf("a workspace template restored within %v", k.scope)
	}
	k.n = MaxKept - 1
	if err := Restore(ctx, Template{ID: uuid.NewString(), Name: "Bug", ProjectID: project.String()}); err != nil {
		t.Errorf("a restore with room: %v", err)
	}
}

// batchRows stands in for the model's batch keep: each write's scope, rows
// and limit. full refuses the batch for room; err refuses it as the store
// would.
type batchRows struct {
	writes int
	scope  *uuid.UUID
	rows   []*model.Template
	limit  int
	full   bool
	err    error
}

func stubBatch(t *testing.T) *batchRows {
	saved := addRows
	t.Cleanup(func() { addRows = saved })
	b := &batchRows{}
	addRows = func(_ context.Context, scope *uuid.UUID, rows []*model.Template, limit int) (bool, error) {
		b.writes, b.scope, b.rows, b.limit = b.writes+1, scope, rows, limit
		if b.full || b.err != nil {
			return false, b.err
		}
		for _, r := range rows {
			r.ID = uuid.New()
		}
		return true, nil
	}
	return b
}

func TestSaveAllKeepsAScopesTemplatesInOneWrite(t *testing.T) {
	b := stubBatch(t)
	ctx, project, bug, by := context.Background(), uuid.New(), uuid.New(), uuid.New()
	in := []Template{
		{ID: "ignored", ProjectID: uuid.NewString(), Name: " Bug report ", DefaultFor: bug.String(),
			Shape: Shape{Task: projectTemplate.Task{Priority: "high"}}},
		{Name: "Feature brief", Shape: Shape{Task: projectTemplate.Task{Subtasks: []projectTemplate.Subtask{{Name: "Spec"}}}}},
	}
	got, err := SaveAll(ctx, project.String(), in, by)
	if err != nil {
		t.Fatal(err)
	}
	if b.writes != 1 || b.scope == nil || *b.scope != project || b.limit != MaxKept || len(b.rows) != 2 || len(got) != 2 {
		t.Fatalf("%d writes to %v under %d keeping %d, answered %d; want one write of both to the project under %d",
			b.writes, b.scope, b.limit, len(b.rows), len(got), MaxKept)
	}
	for i, r := range b.rows {
		if r.ProjectID == nil || *r.ProjectID != project || r.CreatedBy != by {
			t.Errorf("row %d is kept in %v by %v", i, r.ProjectID, r.CreatedBy)
		}
		if got[i].ID != r.ID.String() || got[i].ProjectID != project.String() {
			t.Errorf("template %d answered as %+v, kept as %v", i, got[i], r.ID)
		}
	}
	if b.rows[0].Name != "Bug report" || got[0].Shape.Type != bug.String() || got[1].Name != "Feature brief" {
		t.Errorf("each is kept as its check leaves it, in order: %+v", got)
	}

	// The workspace's are kept with no project.
	if _, err := SaveAll(ctx, "", in[1:], by); err != nil || b.writes != 2 || b.scope != nil {
		t.Errorf("the workspace's: %v, write %d in %v", err, b.writes, b.scope)
	}
	// None to keep writes nothing.
	if got, err := SaveAll(ctx, project.String(), nil, by); err != nil || len(got) != 0 || b.writes != 2 {
		t.Errorf("none: %v, %v, after %d writes", got, err, b.writes)
	}
}

func TestSaveAllKeepsNoneWhenOneCant(t *testing.T) {
	b := stubBatch(t)
	ctx, project, by := context.Background(), uuid.NewString(), uuid.New()
	two := []Template{{Name: "One"}, {Name: "Two"}}
	_, err := SaveAll(ctx, project, []Template{{Name: "Fine"}, {Name: " "}}, by)
	wantFix(t, "one with no name", err, "name")
	_, err = SaveAll(ctx, "not a project", two, by)
	wantFix(t, "a scope that isn't a project", err, "project")
	if b.writes != 0 {
		t.Fatalf("%d writes for sets that fail their check", b.writes)
	}
	b.full = true
	_, err = SaveAll(ctx, project, two, by)
	wantFix(t, "a full project", err, "100", "project")
	_, err = SaveAll(ctx, "", two, by)
	wantFix(t, "a full workspace", err, "100", "workspace")
	b.full = false
	b.err = model.ErrNameTaken
	_, err = SaveAll(ctx, project, two, by)
	wantFix(t, "a name taken", err, "name")
	b.err = model.ErrDefaultTaken
	_, err = SaveAll(ctx, project, two, by)
	wantFix(t, "a default taken", err, "default")
	b.err = errors.New("connection reset")
	if _, err = SaveAll(ctx, project, two, by); err == nil || errors.As(err, new(*TemplateError)) {
		t.Errorf("a failed write: %v, want the error as it is", err)
	}
}
