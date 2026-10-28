//go:build integration

package integration_test

// A task goes only to someone who may see it. An assignee reads their task,
// so a task given to someone outside its project, or to one of its people a
// restriction keeps out, let them in: Maya, the assignee of a task Olu
// restricted, could hand it to Kim, whom the restriction keeps out, and Kim
// read it. Each path refuses in words and changes nothing: handing a task
// over, making one, making a sub-task, and a script through a token. The
// project's people, and the people let in on a restricted task, still get it.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestATaskGoesOnlyToSomeoneWhoMaySeeIt$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATaskGoesOnlyToSomeoneWhoMaySeeIt(t *testing.T) {
	ctx := context.Background()
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	names := map[string]string{"olu": "Olu Owner", "maya": "Maya Lin", "kim": "Kim Park", "gail": "Gail Ford", "ben": "Ben Ade"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for key, name := range names {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@sees.test', $2, $3, NOW(), NOW())`, id, key, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	refs := func(keys ...string) []map[string]any {
		var out []map[string]any
		for _, k := range keys {
			out = append(out, map[string]any{"uid": "_:" + k})
		}
		return out
	}
	// Olu runs the launch; Maya, Kim and Gail work on it; Ben is in the team,
	// not the project. The vault is restricted: Olu let Gail in, and it's Maya's.
	launch, kit, vault := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, launch, people["olu"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, kit, vault, launch, people["olu"])
	exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, vault, launch, people["olu"])
	exec(`INSERT INTO task_restriction_grants (task_id, user_id, granted_by) VALUES ($1, $2, $3)`, vault, people["gail"], people["olu"])
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Core",
			"team_members": refs("olu", "maya", "kim", "gail", "ben"), "team_admins": refs("olu")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"}, "project_members": refs("olu", "maya", "kim", "gail"),
			"project_admins": refs("olu"), "project_created_by": map[string]any{"uid": "_:olu"}, "project_tasks": refs("kit", "vault")},
		map[string]any{"uid": "_:kit", "dgraph.type": "Task", "task_uuid": kit.String(), "task_name": "Press kit", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"}},
		map[string]any{"uid": "_:vault", "dgraph.type": "Task", "task_uuid": vault.String(), "task_name": "Pricing", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:maya"}},
	)
	dg.Mutate(t, nodes)

	// as is who's asking: in the app, or through an API token.
	as := func(key string, token bool) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[key])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", key, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[key].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", key, err)
		}
		c := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
		if token {
			c = helpers.WithActsThroughToken(c)
		}
		return c
	}
	olu, maya, mayasScript := as("olu", false), as("maya", false), as("maya", true)
	call := func(handler http.HandlerFunc, who context.Context, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(who))
		var answer struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Msg
	}
	assignee := func(task uuid.UUID) string {
		t.Helper()
		var out struct {
			T []struct {
				A *struct {
					ID string `json:"user_uuid"`
				} `json:"task_assignee"`
			} `json:"t"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ t(func: eq(task_uuid, "`+task.String()+`")) { task_uuid task_assignee { user_uuid } } }`), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read task %s's assignee: %v", task, err)
		}
		if out.T[0].A == nil {
			return ""
		}
		return out.T[0].A.ID
	}
	hand := func(task uuid.UUID, to string) map[string]any {
		return map[string]any{"task_uuid": task.String(), "task_project_uuid": launch.String(), "task_assignee_uuid": people[to].String()}
	}
	outside := "Ben Ade can't see this task. Add them to the project first."
	keptOut := "Kim Park can't see this task: only some people can see it. A project admin can let them in."

	t.Run("each way of giving a task to someone who can't see it is refused in words, and nothing changes", func(t *testing.T) {
		for _, c := range []struct {
			what    string
			handler http.HandlerFunc
			who     context.Context
			body    map[string]any
			says    string
		}{
			{"Maya, its assignee, handing the restricted task to Kim", taskController.UpdateTaskAssignee, maya, hand(vault, "kim"), keptOut},
			{"Olu handing the press kit to Ben, outside the project", taskController.UpdateTaskAssignee, olu, hand(kit, "ben"), outside},
			{"Olu making a task for Ben", taskController.CreateTask, olu, map[string]any{"task_project_uuid": launch.String(), "task_name": "Venue", "task_assignee_uuid": people["ben"].String()}, outside},
			{"Olu making a sub-task of the restricted task for Kim", taskController.CreateSubTask, olu, map[string]any{"task_uuid": vault.String(), "task_project_uuid": launch.String(), "task_name": "Numbers", "task_assignee_uuid": people["kim"].String()}, keptOut},
			{"Maya's script making a task for Ben", v1Controller.CreateTaskV1, mayasScript, map[string]any{"task_name": "Venue", "project_uuid": launch.String(), "assignee_uuid": people["ben"].String()}, outside},
		} {
			if code, said := call(c.handler, c.who, c.body); code != http.StatusConflict || said != c.says {
				t.Errorf("%s: %d %q, want %d %q", c.what, code, said, http.StatusConflict, c.says)
			}
		}
		if got := assignee(vault); got != people["maya"].String() {
			t.Errorf("the restricted task is assigned to %q after the refusals, not Maya", got)
		}
		if got := assignee(kit); got != "" {
			t.Errorf("the press kit is assigned to %q after the refusal", got)
		}
		var tasks int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id = $1`, launch).Scan(&tasks); err != nil || tasks != 2 {
			t.Errorf("tasks in the project after the refusals: %d %v, want the 2 it had", tasks, err)
		}
	})

	t.Run("the project's people, and someone let in on a restricted task, are given it", func(t *testing.T) {
		for _, c := range []struct {
			what    string
			handler http.HandlerFunc
			who     context.Context
			body    map[string]any
		}{
			{"Olu handing the press kit to Kim", taskController.UpdateTaskAssignee, olu, hand(kit, "kim")},
			{"Maya handing the restricted task to Gail, whom Olu let in", taskController.UpdateTaskAssignee, maya, hand(vault, "gail")},
			{"Maya's script making a task for Kim", v1Controller.CreateTaskV1, mayasScript, map[string]any{"task_name": "Venue", "project_uuid": launch.String(), "assignee_uuid": people["kim"].String()}},
		} {
			if code, said := call(c.handler, c.who, c.body); code != http.StatusOK {
				t.Errorf("%s: %d %q", c.what, code, said)
			}
		}
		if got := assignee(vault); got != people["gail"].String() {
			t.Errorf("the restricted task went to %q, not Gail", got)
		}
		if got := assignee(kit); got != people["kim"].String() {
			t.Errorf("the press kit went to %q, not Kim", got)
		}
	})
}
