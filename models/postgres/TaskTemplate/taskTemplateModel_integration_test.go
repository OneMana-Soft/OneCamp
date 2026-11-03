//go:build integration

package models

// Task templates against a real Postgres 12 with every migration applied.
// Run: go test -tags=integration ./models/postgres/TaskTemplate/ -v

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) { os.Exit(integration.Main(m)) }

func names(list []Template) []string {
	out := []string{}
	for _, t := range list {
		scope := "workspace"
		if t.ProjectID != nil {
			scope = t.ProjectID.String()[:4]
		}
		out = append(out, scope+":"+t.Name)
	}
	return out
}

// Templates are kept per project and for the workspace: a name and a type's
// default are taken once in each, among the live ones; a project's list is
// its own, then the workspace's, without bodies; deleting is soft, frees the
// name and the default, and is undone by a restore unless a live template has
// taken them meanwhile.
func TestTaskTemplatesAreKeptPerScopeAndDeletedSoftly(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	p1, p2, author, admin := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	bug, feature := uuid.New(), uuid.New()
	add := func(project *uuid.UUID, name string, def *uuid.UUID) (*Template, error) {
		tpl := &Template{ProjectID: project, Name: name, Body: []byte(`{"name":"` + name + `"}`), DefaultFor: def, CreatedBy: author}
		return tpl, Add(ctx, tpl)
	}
	report, err := add(&p1, "Bug report", &bug)
	if err != nil || report.ID == uuid.Nil || report.CreatedAt.IsZero() {
		t.Fatalf("add: %v, %+v", err, report)
	}
	if _, err := add(&p1, "BUG REPORT", nil); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a name taken in the project, in another case: %v", err)
	}
	if _, err := add(&p1, "Crash", &bug); !errors.Is(err, ErrDefaultTaken) {
		t.Errorf("a second default for Bug in the project: %v", err)
	}
	if _, err := add(&p2, "Bug report", &bug); err != nil {
		t.Errorf("the same name and default in another project: %v", err)
	}
	ws, err := add(nil, "Bug report", &bug)
	if err != nil {
		t.Fatalf("the same name and default in the workspace: %v", err)
	}
	if _, err := add(nil, "bug report", nil); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a name taken in the workspace: %v", err)
	}
	if _, err := add(&p1, "Feature", &feature); err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, p1)
	if err != nil {
		t.Fatal(err)
	}
	got, want := names(list), []string{p1.String()[:4] + ":Bug report", p1.String()[:4] + ":Feature", "workspace:Bug report"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("p1's list is %v, want %v", got, want)
	}
	for _, tpl := range list {
		if tpl.Body != nil {
			t.Errorf("a list carries the body of %q", tpl.Name)
		}
	}
	if wsList, err := ListWorkspace(ctx); err != nil || len(wsList) != 1 || wsList[0].ID != ws.ID {
		t.Errorf("the workspace's list: %v, %v", err, names(wsList))
	}
	if n, err := Count(ctx, &p1); err != nil || n != 2 {
		t.Errorf("p1 counts %d (%v), want 2", n, err)
	}
	if n, err := Count(ctx, nil); err != nil || n != 1 {
		t.Errorf("the workspace counts %d (%v), want 1", n, err)
	}

	report.Name, report.Description, report.DefaultFor = "Bug", "For crashes", nil
	if err := Update(ctx, report, admin); err != nil {
		t.Fatal(err)
	}
	read, err := Get(ctx, report.ID)
	if err != nil || read == nil || read.Name != "Bug" || read.Description != "For crashes" || read.DefaultFor != nil ||
		read.UpdatedBy == nil || *read.UpdatedBy != admin || string(read.Body) == "" {
		t.Fatalf("read back after the change: %v, %+v", err, read)
	}
	if _, err := add(&p1, "Crash", &bug); err != nil {
		t.Errorf("Bug's default, freed by the change, is taken again: %v", err)
	}

	if err := Delete(ctx, report.ID, admin); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, report.ID, admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a deleted template: %v", err)
	}
	gone, err := Get(ctx, report.ID)
	if err != nil || gone == nil || gone.DeletedAt == nil || gone.DeletedBy == nil || *gone.DeletedBy != admin {
		t.Fatalf("a deleted template is still read, with who and when: %v, %+v", err, gone)
	}
	if list, _ := List(ctx, p1); len(list) != 3 {
		t.Errorf("a deleted template is left out of the list: %v", names(list))
	}
	again, err := add(&p1, "bug", nil)
	if err != nil {
		t.Fatalf("a deleted template's name is free: %v", err)
	}
	if err := restore(ctx, report.ID); !errors.Is(err, ErrNameTaken) {
		t.Errorf("restoring over a live template of the same name: %v", err)
	}
	if err := Delete(ctx, again.ID, admin); err != nil {
		t.Fatal(err)
	}
	if err := restore(ctx, report.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if back, _ := Get(ctx, report.ID); back == nil || back.DeletedAt != nil || back.DeletedBy != nil {
		t.Errorf("a restored template is live again: %+v", back)
	}
	if err := restore(ctx, report.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring a live template: %v", err)
	}
	if missing, err := Get(ctx, uuid.New()); err != nil || missing != nil {
		t.Errorf("an unknown id reads %+v, %v", missing, err)
	}
}

// A restore keeps its scope's cap as an add does (CR-167 F3): with the project
// full, a deleted template stays deleted, and comes back once there is room.
func TestARestoreKeepsItsScopesCap(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, author := uuid.New(), uuid.New()
	add := func(name string) *Template {
		t.Helper()
		tpl := &Template{ProjectID: &project, Name: name, Body: []byte(`{"name":"` + name + `"}`), CreatedBy: author}
		if added, err := AddWithin(ctx, tpl, 2); err != nil || !added {
			t.Fatalf("add %s: %v, %v", name, added, err)
		}
		return tpl
	}
	first, second := add("First"), add("Second")
	if err := Delete(ctx, first.ID, author); err != nil {
		t.Fatal(err)
	}
	add("Third")
	if restored, err := RestoreWithin(ctx, first.ID, &project, 2); err != nil || restored {
		t.Fatalf("a restore into a full project: %v, %v; want refused", restored, err)
	}
	if n, err := Count(ctx, &project); err != nil || n != 2 {
		t.Errorf("the project keeps %d live (%v), want 2", n, err)
	}
	if err := Delete(ctx, second.ID, author); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreWithin(ctx, first.ID, &project, 2); err != nil || !restored {
		t.Fatalf("a restore with room: %v, %v", restored, err)
	}
	if back, _ := Get(ctx, first.ID); back == nil || back.DeletedAt != nil {
		t.Errorf("a restored template is live again: %+v", back)
	}
}

// A set of templates is kept in one statement, all or none: past the scope's
// cap, or with a name already taken, none of it is kept.
func TestTemplatesAreKeptTogetherOrNotAtAll(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, author, bug := uuid.New(), uuid.New(), uuid.New()
	row := func(name string, scope *uuid.UUID) *Template {
		return &Template{ProjectID: scope, Name: name, Description: "About " + name, Body: []byte(`{"name":"` + name + `"}`), CreatedBy: author}
	}
	first, second := row("Bug report", &project), row("Feature brief", &project)
	first.DefaultFor = &bug
	if added, err := AddAllWithin(ctx, &project, []*Template{first, second}, 3); err != nil || !added {
		t.Fatalf("two into a project with room: %v, %v", added, err)
	}
	for _, r := range []*Template{first, second} {
		got, err := Get(ctx, r.ID)
		if err != nil || got == nil {
			t.Fatalf("%s (%v) reads back as nothing: %v", r.Name, r.ID, err)
		}
		var body struct{ Name string }
		if got.Name != r.Name || got.Description != r.Description || got.ProjectID == nil || *got.ProjectID != project ||
			got.CreatedBy != author || r.CreatedAt.IsZero() || !got.CreatedAt.Equal(r.CreatedAt) ||
			json.Unmarshal(got.Body, &body) != nil || body.Name != r.Name {
			t.Errorf("%s reads back as %+v (%s); kept as %+v", r.Name, got, got.Body, r)
		}
	}
	if got, _ := Get(ctx, first.ID); got == nil || got.DefaultFor == nil || *got.DefaultFor != bug {
		t.Errorf("the default reads back as %+v", got)
	}
	if got, _ := Get(ctx, second.ID); got == nil || got.DefaultFor != nil {
		t.Errorf("no default reads back as %+v", got)
	}

	// Two more don't fit in the one place left: neither is kept.
	if added, err := AddAllWithin(ctx, &project, []*Template{row("Third", &project), row("Fourth", &project)}, 3); err != nil || added {
		t.Fatalf("two into one place left: %v, %v; want refused", added, err)
	}
	// A name already taken, in any case, keeps none of the set.
	if _, err := AddAllWithin(ctx, &project, []*Template{row("Fifth", &project), row("BUG REPORT", &project)}, 10); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a taken name: %v, want ErrNameTaken", err)
	}
	if n, err := Count(ctx, &project); err != nil || n != 2 {
		t.Errorf("the project keeps %d live (%v), want 2", n, err)
	}

	// The workspace's are kept with no project, apart from the project's.
	ws := row("Bug report", nil)
	if added, err := AddAllWithin(ctx, nil, []*Template{ws}, 3); err != nil || !added || ws.ID == uuid.Nil {
		t.Fatalf("the workspace's: %v, %v, %v", added, err, ws.ID)
	}
	if got, _ := Get(ctx, ws.ID); got == nil || got.ProjectID != nil {
		t.Errorf("the workspace's reads back as %+v", got)
	}
}
