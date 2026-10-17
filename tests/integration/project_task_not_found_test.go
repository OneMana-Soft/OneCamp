//go:build integration
// +build integration

package integration_test

// A project or task id that names nothing answers 404, as a task its reader
// may not see does; a project the reader isn't in stays 403 (the web app says
// "You can't see this project"); and a read that failed is a 503, which the
// app offers Try again for. Every one of these reads answered 400 for an id
// that names nothing, which the app could only call a connection problem
// (PTC-1, PTC-4), and the same 400 for a read that failed.
//
// Run: go test -tags=integration ./tests/integration/ -run TestProjectAndTaskReadsSayNotFound -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestProjectAndTaskReadsSayNotFound(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Ana runs the project; Cy is in the team but not in the project.
	people := map[string]uuid.UUID{}
	nodes := []map[string]any{}
	for _, name := range []string{"ana", "cy"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	project, task := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, project, people["ana"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, people["ana"])
	exec(`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, 'all')`, people["ana"], project)
	live := "0001-01-01T00:00:00Z"
	nodes = append(nodes,
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Acme",
			"team_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:cy"}}, "team_admins": []map[string]any{{"uid": "_:ana"}}},
		map[string]any{"uid": "_:website", "dgraph.type": "Project", "project_uuid": project, "project_name": "Website",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:team"},
			"project_members": []map[string]any{{"uid": "_:ana"}}, "project_admins": []map[string]any{{"uid": "_:ana"}},
			"project_created_by": map[string]any{"uid": "_:ana"},
			"project_tasks": []map[string]any{{"dgraph.type": "Task", "task_uuid": task, "task_name": "Fix login", "task_status": "todo",
				"task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ana"}}}},
	)
	dg.Mutate(t, nodes)

	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	asAna, asCy := as("ana"), as("cy")
	get := func(who context.Context, h http.HandlerFunc, param, id string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add(param, id)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/?pageSize=50&pageIndex=0", nil).WithContext(context.WithValue(who, chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	reads := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"info", projectController.ProjectInfo},
		{"task list", projectController.GetProjectTaskList},
		{"board", projectController.GetProjectTaskListForKanban},
		{"members", projectController.GetDgraphProjectMemberList},
		{"members with their roles", projectController.GetDgraphProjectCombinedMemberInfo},
		{"files", projectController.GetDgraphProjectAttachmentList},
	}
	unknown := uuid.NewString()

	t.Run("a member reads the project", func(t *testing.T) {
		for _, rd := range reads {
			if code, body := get(asAna, rd.h, "project_uuid", project); code != http.StatusOK {
				t.Errorf("%s as a member: %d %s", rd.name, code, body)
			}
		}
	})

	t.Run("a project id that names nothing is not found", func(t *testing.T) {
		for _, rd := range reads {
			if code, body := get(asAna, rd.h, "project_uuid", unknown); code != http.StatusNotFound {
				t.Errorf("%s of a project that isn't there: %d %s, want 404", rd.name, code, body)
			}
		}
	})

	t.Run("a project the reader isn't in stays refused", func(t *testing.T) {
		for _, rd := range reads {
			if code, body := get(asCy, rd.h, "project_uuid", project); code != http.StatusForbidden {
				t.Errorf("%s as someone outside the project: %d %s, want 403", rd.name, code, body)
			}
		}
	})

	t.Run("a task id that names nothing is not found, as a task the reader may not see", func(t *testing.T) {
		if code, body := get(asAna, taskController.GetTaskInfo, "task_uuid", task); code != http.StatusOK {
			t.Fatalf("the task as its project's admin: %d %s", code, body)
		}
		if code, body := get(asAna, taskController.GetTaskInfo, "task_uuid", unknown); code != http.StatusNotFound {
			t.Errorf("a task that isn't there: %d %s, want 404", code, body)
		}
		if code, body := get(asCy, taskController.GetTaskInfo, "task_uuid", task); code != http.StatusNotFound {
			t.Errorf("a task in a project the reader isn't in: %d %s, want 404", code, body)
		}
		if code, body := get(asAna, taskController.GetTaskInfo, "task_uuid", "not-a-task"); code != http.StatusBadRequest {
			t.Errorf("an id that isn't one: %d %s, want 400", code, body)
		}
	})

	t.Run("a read that failed is a 503, not a missing project or task", func(t *testing.T) {
		dg.Close()
		for _, rd := range reads {
			if code, body := get(asAna, rd.h, "project_uuid", project); code != http.StatusServiceUnavailable {
				t.Errorf("%s while Dgraph doesn't answer: %d %s, want 503", rd.name, code, body)
			}
		}
		if code, body := get(asAna, taskController.GetTaskInfo, "task_uuid", task); code != http.StatusServiceUnavailable {
			t.Errorf("a task while Dgraph doesn't answer: %d %s, want 503", code, body)
		}
	})
}
