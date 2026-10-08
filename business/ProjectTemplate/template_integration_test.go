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
		// The client project's own field comes back with it.
		if len(got.Fields) != 1 || got.Fields[0].Name != "Client approved" || got.Fields[0].Type != "checkbox" || !got.Fields[0].OnCard {
			t.Fatalf("fields read back: %+v", got.Fields)
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
		var size int
		if err := env.PG.QueryRow(`SELECT octet_length(body::text) + coalesce(array_length(preview, 1), 0) FROM project_templates WHERE id = $1`, saved.ID).Scan(&size); err != nil || size > 2 {
			t.Fatalf("a deleted template keeps its plan: %d bytes, %v", size, err)
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
	projectUID := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch"})["p"]
	// add makes a task in its own write, as the app makes each one, so uids
	// follow the order of the calls. Every task goes on the project's list,
	// subtasks too, as the app puts them; a subtask also hangs off its parent.
	add := func(task map[string]any, parent string) string {
		task["uid"], task["dgraph.type"] = "_:t", "Task"
		if parent != "" {
			task["task_parent_task"] = map[string]any{"uid": parent}
		}
		uid := dg.Mutate(t, task)["t"]
		dg.Mutate(t, map[string]any{"uid": projectUID, "project_tasks": []map[string]any{{"uid": uid}}})
		if parent != "" {
			dg.Mutate(t, map[string]any{"uid": parent, "task_sub_tasks": []map[string]any{{"uid": uid}}})
		}
		return uid
	}
	// Creation times run against the order the tasks were made, as they do in
	// a project made from a template (readingOrder): the read must follow the
	// making, not the times.
	plan := add(map[string]any{"task_uuid": uuid.NewString(), "task_name": "Plan", "task_status": "todo",
		"task_deleted_at": live, "task_created_at": "2026-03-09T09:00:00Z", "task_due_date": "2026-03-04T17:00:00Z"}, "")
	add(map[string]any{"task_name": "step one", "task_deleted_at": live, "task_created_at": "2026-03-08T10:00:00Z"}, plan)
	add(map[string]any{"task_name": "removed step", "task_deleted_at": gone, "task_created_at": "2026-03-07T11:00:00Z"}, plan)
	add(map[string]any{"task_name": "step two", "task_deleted_at": live, "task_created_at": "2026-03-06T11:00:00Z"}, plan)
	add(map[string]any{"task_uuid": uuid.NewString(), "task_name": "Build", "task_status": "inProgress",
		"task_deleted_at": live, "task_created_at": "2026-03-02T09:00:00Z"}, "")
	add(map[string]any{"task_uuid": uuid.NewString(), "task_name": "Deleted", "task_status": "todo",
		"task_deleted_at": gone, "task_created_at": "2026-03-01T09:00:00Z"}, "")

	p, err := projectDomain.GetDgraphProjectTasksForTemplate(ctx, project, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.TaskCount != 2 || len(p.Tasks) != 2 || p.Tasks[0].Name != "Plan" || p.Tasks[1].Name != "Build" {
		t.Fatalf("live top-level tasks, in the order they were made: count %d, %+v", p.TaskCount, p.Tasks)
	}
	if subs := p.Tasks[0].SubTasks; len(subs) != 2 || subs[0].Name != "step one" || subs[1].Name != "step two" {
		t.Fatalf("live subtasks under their task, in the order they were made: %+v", subs)
	}
	tpl := FromProject("Launch", "", nil, p.Tasks, time.UTC)
	if tpl.Size() != 4 || tpl.Tasks[0].Name != "Plan" || *tpl.Tasks[0].DueDay != 0 || tpl.Tasks[0].Subtasks[1].Name != "step two" {
		t.Fatalf("as a template: %+v", tpl)
	}

	capped, err := projectDomain.GetDgraphProjectTasksForTemplate(ctx, project, 1)
	if err != nil || len(capped.Tasks) != 1 || capped.TaskCount != 2 {
		t.Fatalf("first caps the read, the count doesn't: %+v %v", capped, err)
	}
}
