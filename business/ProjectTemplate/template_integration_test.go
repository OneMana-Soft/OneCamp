//go:build integration

package business

// Saved templates against a real Postgres 12 with every migration applied, and
// the project a template is made from against a real Dgraph with the
// production schema.
// Run: go test -tags=integration ./business/ProjectTemplate/ -v

import (
	"context"
	"errors"
	"testing"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func person(admin bool) *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserPostgresInfo.Id = uuid.New()
	u.UserPostgresInfo.IsAdmin = admin
	return u
}

func TestSavedTemplates(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	author, someone, admin := person(false), person(false), person(true)

	tpl := *builtIn("client-project")
	tpl.Name = "Retainer client"
	saved, err := Save(ctx, tpl, author)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TaskCount != tpl.Size() || len(saved.Preview) != previewSize || !saved.CanDelete || saved.BuiltIn {
		t.Fatalf("saved summary: %+v", saved)
	}

	t.Run("a name is taken whatever its case", func(t *testing.T) {
		tpl.Name = "RETAINER client"
		_, err := Save(ctx, tpl, someone)
		templateError(t, err, "already exists")
	})

	t.Run("listed after the built-in ones, deletable by its author and admins only", func(t *testing.T) {
		for who, want := range map[*userModels.UserInfo]bool{author: true, someone: false, admin: true} {
			list, err := List(ctx, who)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != len(builtins)+1 || !list[0].BuiltIn || list[len(list)-1].ID != saved.ID {
				t.Fatalf("list: %+v", list)
			}
			if got := list[len(list)-1].CanDelete; got != want {
				t.Errorf("can_delete %v, want %v", got, want)
			}
		}
	})

	t.Run("read back whole", func(t *testing.T) {
		got, err := Get(ctx, saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "Retainer client" || len(got.Tasks) != len(tpl.Tasks) || len(got.Statuses) != 1 ||
			got.Tasks[1].Subtasks[0].Name != tpl.Tasks[1].Subtasks[0].Name || *got.Tasks[0].DueDay != *tpl.Tasks[0].DueDay {
			t.Fatalf("read back: %+v", got)
		}
		if _, err := Get(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown id: %v", err)
		}
		if _, err := Get(ctx, "not-a-template"); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown slug: %v", err)
		}
	})

	t.Run("deleted by its author, after which the name is free", func(t *testing.T) {
		if err := Delete(ctx, saved.ID, someone); !errors.Is(err, ErrNotYours) {
			t.Fatalf("someone else: %v", err)
		}
		if err := Delete(ctx, saved.ID, author); err != nil {
			t.Fatal(err)
		}
		if err := Delete(ctx, saved.ID, author); !errors.Is(err, ErrNotFound) {
			t.Fatalf("twice: %v", err)
		}
		if err := Delete(ctx, "client-project", admin); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a built-in one: %v", err)
		}
		tpl.Name = "Retainer client"
		if _, err := Save(ctx, tpl, someone); err != nil {
			t.Fatalf("the name is free again: %v", err)
		}
	})

	t.Run("a template that can't be used isn't kept", func(t *testing.T) {
		_, err := Save(ctx, Template{Name: "Empty"}, author)
		templateError(t, err, "at least one task")
	})
}

func TestProjectAsTemplateQuery(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	project := uuid.NewString()
	live, gone := "0001-01-01T00:00:00Z", time.Now().UTC().Format(time.RFC3339)
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch",
		"project_tasks": []map[string]any{
			{"uid": "_:a", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Plan", "task_status": "todo",
				"task_deleted_at": live, "task_created_at": "2026-03-01T09:00:00Z", "task_due_date": "2026-03-04T17:00:00Z",
				"task_sub_tasks": []map[string]any{
					{"uid": "_:s1", "dgraph.type": "Task", "task_name": "step one", "task_deleted_at": live, "task_created_at": "2026-03-01T10:00:00Z", "task_parent_task": map[string]any{"uid": "_:a"}},
					{"uid": "_:s2", "dgraph.type": "Task", "task_name": "removed step", "task_deleted_at": gone, "task_created_at": "2026-03-01T11:00:00Z", "task_parent_task": map[string]any{"uid": "_:a"}},
				}},
			{"uid": "_:b", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Build", "task_status": "inProgress",
				"task_deleted_at": live, "task_created_at": "2026-03-02T09:00:00Z"},
			{"uid": "_:c", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Deleted", "task_status": "todo",
				"task_deleted_at": gone, "task_created_at": "2026-03-03T09:00:00Z"},
		},
	})
	// Subtasks are on the project's task list too, as the app makes them.
	dg.Mutate(t, map[string]any{"uid": uids["p"], "project_tasks": []map[string]any{{"uid": uids["s1"]}, {"uid": uids["s2"]}}})

	p, err := projectDomain.GetDgraphProjectTasksForTemplate(ctx, project, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.TaskCount != 2 || len(p.Tasks) != 2 || p.Tasks[0].Name != "Plan" || p.Tasks[1].Name != "Build" {
		t.Fatalf("live top-level tasks, oldest first: count %d, %+v", p.TaskCount, p.Tasks)
	}
	if len(p.Tasks[0].SubTasks) != 1 || p.Tasks[0].SubTasks[0].Name != "step one" {
		t.Fatalf("live subtasks under their task: %+v", p.Tasks[0].SubTasks)
	}
	tpl := FromProject("Launch", "", nil, p.Tasks, time.UTC)
	if tpl.Size() != 3 || tpl.Tasks[0].Name != "Plan" || *tpl.Tasks[0].DueDay != 0 {
		t.Fatalf("as a template: %+v", tpl)
	}

	capped, err := projectDomain.GetDgraphProjectTasksForTemplate(ctx, project, 1)
	if err != nil || len(capped.Tasks) != 1 || capped.TaskCount != 2 {
		t.Fatalf("first caps the read, the count doesn't: %+v %v", capped, err)
	}
}
