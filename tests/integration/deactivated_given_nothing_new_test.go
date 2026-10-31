//go:build integration
// +build integration

package integration_test

// A deactivated person is given no new work and no new place (audit C5).
//
// Deactivation writes a deletion time and stops: memberships and assignments
// stay, so reactivating someone gives everything back. Nothing refused NEW
// ones: an admin, a script with a token or an agent could hand a task to
// someone who would never see it, or add them to a channel, a team or a
// project, and nobody was told.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADeactivatedPersonIsGivenNothingNew -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	"github.com/akashc777/OneCamp/business/Formula"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADeactivatedPersonIsGivenNothingNew(t *testing.T) {
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
	names := map[string]string{"olu": "Olu Owner", "maya": "Maya Lin", "ben": "Ben Ade", "sam": "Sam Rivera"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for key, name := range names {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $3, NOW(), NOW())`, id, key, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	refs := func(keys ...string) []map[string]any {
		var out []map[string]any
		for _, k := range keys {
			out = append(out, map[string]any{"uid": "_:" + k})
		}
		return out
	}
	// Olu runs everything; Maya works on the launch. Ben and Sam are in the
	// Core team only, and Sam has the brief from before he left.
	launch, kit, brief, channel, design := uuid.New(), uuid.New(), uuid.New(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, launch, people["olu"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, kit, brief, launch, people["olu"])
	exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'launch', false, $2)`, channel, people["olu"])
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Core",
			"team_members": refs("olu", "maya", "ben", "sam"), "team_admins": refs("olu")},
		map[string]any{"uid": "_:design", "dgraph.type": "Team", "team_uuid": design, "team_name": "Design",
			"team_members": refs("olu"), "team_admins": refs("olu")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"}, "project_members": refs("olu", "maya"),
			"project_admins": refs("olu"), "project_created_by": map[string]any{"uid": "_:olu"}, "project_tasks": refs("kit", "brief")},
		map[string]any{"uid": "_:kit", "dgraph.type": "Task", "task_uuid": kit.String(), "task_name": "Press kit", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"}},
		map[string]any{"uid": "_:brief", "dgraph.type": "Task", "task_uuid": brief.String(), "task_name": "Brief", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:sam"}},
		map[string]any{"uid": "_:channel", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": live, "ch_members": refs("olu"), "ch_moderators": refs("olu")},
	)
	dg.Mutate(t, nodes)

	// A form Olu made while Sam was here, which gives what it files to him.
	samID := people["sam"]
	fields, _ := json.Marshal([]formBusiness.Field{{Id: "f1", Label: "What happened?", Type: "text", Required: true}})
	form, err := formModel.Create(ctx, formModel.Form{ProjectUUID: launch, Token: "cdefghijklmnopqrstuvwxyz", Title: "Report a bug",
		Fields: fields, Priority: "medium", Active: true, CreatedBy: people["olu"], AssigneeUUID: &samID})
	if err != nil {
		t.Fatalf("create the form: %v", err)
	}

	// Sam leaves, as an admin deactivates anyone.
	if err := userBusiness.DeactivateUser(ctx, samID); err != nil {
		t.Fatalf("deactivate Sam: %v", err)
	}

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
	olu, mayasScript := as("olu", false), as("maya", true)
	// call runs handler as who with body, and answers the status and the msg
	// the web shows.
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
		// task_uuid too: Dgraph leaves out a node none of whose asked-for
		// predicates it has, so an unassigned task came back as no task.
		if err := json.Unmarshal(dg.Query(t, `{ t(func: eq(task_uuid, "`+task.String()+`")) { task_uuid task_assignee { user_uuid } } }`), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read task %s's assignee: %v", task, err)
		}
		if out.T[0].A == nil {
			return ""
		}
		return out.T[0].A.ID
	}
	sam, ben := samID.String(), people["ben"].String()
	assign := func(task uuid.UUID, to string) map[string]any {
		return map[string]any{"task_uuid": task.String(), "task_project_uuid": launch.String(), "task_assignee_uuid": to}
	}

	for _, c := range []struct {
		what    string
		handler http.HandlerFunc
		who     context.Context
		body    map[string]any
		as      string
	}{
		{"Olu handing Sam the press kit", taskController.UpdateTaskAssignee, olu, assign(kit, sam), "assigned"},
		{"Olu making a task for Sam", taskController.CreateTask, olu, map[string]any{"task_project_uuid": launch.String(), "task_name": "Venue", "task_assignee_uuid": sam}, "assigned"},
		{"Olu making a sub-task for Sam", taskController.CreateSubTask, olu, map[string]any{"task_uuid": kit.String(), "task_project_uuid": launch.String(), "task_name": "Quotes", "task_assignee_uuid": sam}, "assigned"},
		{"Maya's script making a task for Sam", v1Controller.CreateTaskV1, mayasScript, map[string]any{"task_name": "Venue", "project_uuid": launch.String(), "assignee_uuid": sam}, "assigned"},
		{"Olu adding Sam to the channel", channelController.AddChannelMember, olu, map[string]any{"channel_id": channel, "user_id": sam}, "added"},
		{"Olu adding Sam to the Design team", teamController.AddMemberToTeam, olu, map[string]any{"team_uuid": design, "member_uuid": sam}, "added"},
		{"Olu adding Sam to the project", projectController.AddMemberToProject, olu, map[string]any{"project_uuid": launch.String(), "user_uuid": sam}, "added"},
	} {
		want := "Sam Rivera can't be " + c.as + ": their account is deactivated."
		if code, said := call(c.handler, c.who, c.body); code != http.StatusConflict || said != want {
			t.Errorf("%s: %d %q, want %d %q", c.what, code, said, http.StatusConflict, want)
		}
	}
	if got := assignee(kit); got != "" {
		t.Errorf("the press kit is assigned to %s after the refusal", got)
	}
	var tasks int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id = $1`, launch).Scan(&tasks); err != nil || tasks != 2 {
		t.Errorf("tasks in the project after the refusals: %d %v, want the 2 it had", tasks, err)
	}

	// The same for someone whose account is live goes through: the rule is
	// about Sam's account, not about who asks.
	for _, c := range []struct {
		what    string
		handler http.HandlerFunc
		who     context.Context
		body    map[string]any
	}{
		{"Olu handing Maya the press kit", taskController.UpdateTaskAssignee, olu, assign(kit, people["maya"].String())},
		{"Maya's script making a task for Olu", v1Controller.CreateTaskV1, mayasScript, map[string]any{"task_name": "Venue", "project_uuid": launch.String(), "assignee_uuid": people["olu"].String()}},
		{"Olu adding Ben to the channel", channelController.AddChannelMember, olu, map[string]any{"channel_id": channel, "user_id": ben}},
		{"Olu adding Ben to the Design team", teamController.AddMemberToTeam, olu, map[string]any{"team_uuid": design, "member_uuid": ben}},
		{"Olu adding Ben to the project", projectController.AddMemberToProject, olu, map[string]any{"project_uuid": launch.String(), "user_uuid": ben}},
	} {
		if code, said := call(c.handler, c.who, c.body); code != http.StatusOK {
			t.Errorf("%s: %d %q", c.what, code, said)
		}
	}

	// What Sam had stays his.
	if got := assignee(brief); got != sam {
		t.Errorf("the brief Sam had is assigned to %q", got)
	}

	// His weekly brief, done, comes round again for someone to pick up: the
	// next one isn't his, and it isn't lost.
	tasksNow := func() map[uuid.UUID]bool {
		t.Helper()
		rows, err := env.PG.Query(`SELECT id FROM tasks WHERE project_id = $1`, launch)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		ids := map[uuid.UUID]bool{}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids[id] = true
		}
		return ids
	}
	exec(`INSERT INTO task_recurrences (task_uuid, rule, created_by) VALUES ($1, 'FREQ=WEEKLY', $2)`, brief, people["olu"])
	before := tasksNow()
	if code, said := call(taskController.UpdateTaskStatus, olu, map[string]any{"task_uuid": brief.String(), "task_project_uuid": launch.String(), "task_status": "done"}); code != http.StatusOK {
		t.Fatalf("Olu marking the brief done: %d %q", code, said)
	}
	var next []uuid.UUID
	for id := range tasksNow() {
		if !before[id] {
			next = append(next, id)
		}
	}
	if len(next) != 1 {
		t.Fatalf("the brief came round %d times, want once", len(next))
	}
	if got := assignee(next[0]); got != "" {
		t.Errorf("the next brief is assigned to %s", got)
	}

	// A visitor's answers to his form still become a task: one nobody is
	// given yet, rather than a form that refuses everyone for whom it names.
	if err := formBusiness.Submit(ctx, form.Token, map[string]any{"f1": "It broke"}, formula.NumberSigns{}, "", "", time.Now()); err != nil {
		t.Fatalf("a visitor's answer to a form that gives its tasks to Sam: %v", err)
	}
	var filed uuid.UUID
	if err := env.PG.QueryRow(`SELECT task_uuid FROM form_submissions WHERE form_id = $1`, form.Id).Scan(&filed); err != nil {
		t.Fatalf("the form's task: %v", err)
	}
	if got := assignee(filed); got != "" {
		t.Errorf("the form's task is assigned to %s", got)
	}

	// Reactivated, Sam can be given places and work again: back on the
	// project, its tasks (a task goes only to someone who may see it).
	if err := userBusiness.ActivateUser(ctx, samID); err != nil {
		t.Fatalf("reactivate Sam: %v", err)
	}
	if code, said := call(projectController.AddMemberToProject, olu, map[string]any{"project_uuid": launch.String(), "user_uuid": sam}); code != http.StatusOK {
		t.Errorf("adding Sam to the project once he's back: %d %q", code, said)
	}
	if code, said := call(taskController.UpdateTaskAssignee, olu, assign(kit, sam)); code != http.StatusOK || assignee(kit) != sam {
		t.Errorf("handing Sam the press kit once he's back: %d %q", code, said)
	}
	if code, said := call(channelController.AddChannelMember, olu, map[string]any{"channel_id": channel, "user_id": sam}); code != http.StatusOK {
		t.Errorf("adding Sam to the channel once he's back: %d %q", code, said)
	}
}
