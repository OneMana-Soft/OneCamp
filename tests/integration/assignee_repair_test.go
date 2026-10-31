//go:build integration

package integration_test

// A task goes only to someone who may read it already, and assignments made
// before that rule are brought into line with it. Once, as the server starts
// after the upgrade, each assignee the rule would refuse now is taken off:
// Ben, outside the launch, from the press kit, from a task in the trash
// (which stays there) and from one linked to a GitHub issue (whose own
// assignees nothing is sent to); Kim, one of the launch's people, from the
// restricted vault she isn't let into; Ben from the orphaned project's task
// too. Each line in a task's history names "OneCamp, upgrade repair", and
// after it Ben can't open the press kit. The launch's admin is told in her
// Inbox, once per person, how many tasks and never which; the orphaned
// project, whose only admin left, tells the workspace's admin. Everyone the
// rule would still give their task keeps it: a project member, someone let
// into a restricted task, the project's admin on one, an AI teammate, an
// imported author, and Ben on a task in no project. An install with nothing
// to take off never gets the system principal, and a second pass changes
// nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestAnUpgradeTakesOffAssigneesTheAssignRuleWouldRefuse$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	assigneeRepairBusiness "github.com/akashc777/OneCamp/business/AssigneeRepair"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnUpgradeTakesOffAssigneesTheAssignRuleWouldRefuse(t *testing.T) {
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
	live, trashed := "0001-01-01T00:00:00Z", "2026-10-01T09:00:00Z"
	names := map[string]string{"olu": "Olu Owner", "maya": "Maya Lin", "kim": "Kim Park", "gail": "Gail Ford", "ben": "Ben Ade",
		"dee": "Dee Gone", "wanda": "Wanda Admin", "agent": "Launch helper", "author": "Imported Author"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for key, name := range names {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@repair.test', $2, $3, NOW(), NOW())`, id, key, name)
		node := map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live}
		switch key {
		case "agent":
			node["is_bot"] = true
		case "author":
			node["is_external"] = true
		case "dee":
			node["user_deleted_at"] = trashed
		}
		nodes = append(nodes, node)
	}
	exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, people["dee"])
	exec(`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`, people["wanda"])
	refs := func(keys ...string) []map[string]any {
		var out []map[string]any
		for _, k := range keys {
			out = append(out, map[string]any{"uid": "_:" + k})
		}
		return out
	}
	// Olu runs the launch; Maya, Kim and Gail work on it. The vault, the
	// ledger and the board pack are restricted: Olu let Gail into the vault
	// and the ledger. Dee ran the orphaned project and has left.
	launch, orphan := uuid.New(), uuid.New()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $3), ($2, 'Orphan', $4)`, launch, orphan, people["olu"], people["dee"])
	tasks := map[string]uuid.UUID{}
	for _, key := range []string{"kit", "plan", "vault", "ledger", "board", "old", "issue", "ctrl", "robot", "imported", "moved", "loose", "mine"} {
		tasks[key] = uuid.New()
	}
	// Maya made the board pack, so Olu reads it as the project's admin alone.
	maker := func(key string) string {
		if key == "board" {
			return "maya"
		}
		return "olu"
	}
	for key, id := range tasks {
		project := &launch
		switch key {
		case "loose":
			project = &orphan
		case "mine":
			project = nil
		}
		exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, project, people[maker(key)])
	}
	// An import that ran after the upgrade made the moved card, and records
	// it as its own, made now (its task keeps the source's own date).
	job := uuid.New()
	exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by) VALUES ($1, 'trello', 'Acme', 'api', 'completed', $2)`, job, people["olu"])
	exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'task', 'card-7', $2)`, job, tasks["moved"])
	t.Cleanup(func() { exec(`DELETE FROM import_jobs WHERE id = $1`, job) })
	exec(`UPDATE tasks SET github_issue_url = $2 WHERE id = $1`, tasks["issue"], "https://github.com/acme/launch/issues/7")
	exec(`UPDATE tasks SET github_issue_url = $2 WHERE id = $1`, tasks["ctrl"], "https://github.com/acme/launch/issues/8")
	for _, key := range []string{"vault", "ledger", "board"} {
		exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, tasks[key], launch, people["olu"])
	}
	for _, key := range []string{"vault", "ledger"} {
		exec(`INSERT INTO task_restriction_grants (task_id, user_id, granted_by) VALUES ($1, $2, $3)`, tasks[key], people["gail"], people["olu"])
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Core",
			"team_members": refs("olu", "maya", "kim", "gail", "ben"), "team_admins": refs("olu")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"}, "project_members": refs("olu", "maya", "kim", "gail"),
			"project_admins": refs("olu"), "project_created_by": map[string]any{"uid": "_:olu"},
			"project_tasks": refs("kit", "plan", "vault", "ledger", "board", "old", "issue", "ctrl", "robot", "imported", "moved")},
		map[string]any{"uid": "_:orphan", "dgraph.type": "Project", "project_uuid": orphan.String(), "project_name": "Orphan",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"}, "project_members": refs("dee"),
			"project_admins": refs("dee"), "project_created_by": map[string]any{"uid": "_:dee"}, "project_tasks": refs("loose")},
	)
	// Every task but the ones in no project and the orphan's is the launch's;
	// those assigned well come first, then the old assignments.
	task := func(key, name, deleted string) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": tasks[key].String(), "task_name": name, "task_status": "todo",
			"task_deleted_at": deleted, "task_created_by": map[string]any{"uid": "_:" + maker(key)}}
		switch key {
		case "loose":
			n["task_project"] = map[string]any{"uid": "_:orphan"}
		case "mine":
		default:
			n["task_project"] = map[string]any{"uid": "_:launch"}
		}
		return n
	}
	well := map[string]string{"plan": "maya", "ledger": "gail", "board": "olu", "robot": "agent", "imported": "author", "mine": "ben", "ctrl": "maya"}
	old := map[string]string{"kit": "ben", "vault": "kim", "old": "ben", "issue": "ben", "loose": "ben"}
	for key, name := range map[string]string{"kit": "Press kit", "plan": "Plan", "vault": "Pricing", "ledger": "Ledger", "board": "Board pack",
		"old": "Old venue", "issue": "Fix the form", "ctrl": "Fix the footer", "robot": "Draft the notes", "imported": "Imported card", "moved": "Moved card",
		"loose": "Loose end", "mine": "My own list"} {
		deleted := live
		if key == "old" {
			deleted = trashed
		}
		n := task(key, name, deleted)
		if who, ok := well[key]; ok {
			n["task_assignee"] = map[string]any{"uid": "_:" + who}
		}
		nodes = append(nodes, n)
	}
	uids := dg.Mutate(t, nodes)

	assign := func(assignments map[string]string) {
		t.Helper()
		var set []map[string]any
		for key, who := range assignments {
			set = append(set, map[string]any{"uid": uids[key], "task_assignee": map[string]any{"uid": uids[who]}})
		}
		dg.Mutate(t, set)
	}
	type activity struct {
		Type string `json:"activity_type"`
		Prev string `json:"activity_prev_state"`
		Next string `json:"activity_next_state"`
		By   *struct {
			ID string `json:"user_uuid"`
		} `json:"activity_by"`
	}
	type taskNow struct {
		Assignee *struct {
			ID string `json:"user_uuid"`
		} `json:"task_assignee"`
		Deleted    string     `json:"task_deleted_at"`
		Activities []activity `json:"task_activities"`
	}
	read := func(key string) taskNow {
		t.Helper()
		var out struct {
			T []taskNow `json:"t"`
		}
		q := `{ t(func: eq(task_uuid, "` + tasks[key].String() + `")) { task_deleted_at task_assignee { user_uuid }
			task_activities { activity_type activity_prev_state activity_next_state activity_by { user_uuid } } } }`
		if err := json.Unmarshal(dg.Query(t, q), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read %s: %v", key, err)
		}
		return out.T[0]
	}
	assigneeOf := func(key string) string {
		if a := read(key).Assignee; a != nil {
			return a.ID
		}
		return ""
	}
	principal := func() string {
		t.Helper()
		var id string
		err := env.PG.QueryRow(`SELECT id FROM users WHERE email_id = $1`, userDomain.UpgradeRepairBotEmail).Scan(&id)
		if err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Fatal(err)
		}
		return id
	}
	type notice struct {
		To, Kind, Resource, Actor string
		Data                      map[string]string
	}
	notices := func() []notice {
		t.Helper()
		rows, err := env.PG.Query(`SELECT user_uuid, kind, resource_kind || ':' || resource_uuid, actor_uuid, data::text FROM resource_notices
			WHERE kind = $1 ORDER BY user_uuid, data->>'person', resource_uuid`, assigneeRepairBusiness.NoticeEvent)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []notice
		for rows.Next() {
			var n notice
			var data string
			if err := rows.Scan(&n.To, &n.Kind, &n.Resource, &n.Actor, &data); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(data), &n.Data); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	// opens is whether key's person may open the task, through the app's read.
	opens := func(who, key string) int {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", who, err)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("task_uuid", tasks[key].String())
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		taskController.GetTaskInfo(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(c))
		return rec.Code
	}
	repair := assigneeRepairBusiness.Repair
	again := func() {
		t.Helper()
		exec(`DELETE FROM system_configs WHERE key = $1`, repair.Key)
	}

	t.Run("an install whose assignments all keep the assign rule takes no one off, and never gets the principal", func(t *testing.T) {
		rep, ran, err := repair.Run(ctx)
		if err != nil || !ran || rep.Removed != 0 || rep.Told != 0 || rep.Read != len(well)-1 {
			t.Fatalf("ran %v: %+v %v, want %d read and nobody taken off", ran, rep, err, len(well)-1)
		}
		if id := principal(); id != "" {
			t.Errorf("the system principal was made with nothing to take off: %s", id)
		}
		for key, who := range well {
			if got := assigneeOf(key); got != people[who].String() {
				t.Errorf("%s is assigned to %q, not %s", key, got, who)
			}
		}
	})

	again()
	assign(old)
	// The import gave Ben, outside the launch, its card: a data move's, kept.
	assign(map[string]string{"moved": "ben"})
	if code := opens("ben", "kit"); code != http.StatusOK {
		t.Fatalf("the world before the upgrade isn't as built: Ben's read of the press kit answered %d", code)
	}

	t.Run("the upgrade takes off each assignee the assign rule would refuse now", func(t *testing.T) {
		rep, ran, err := repair.Run(ctx)
		if err != nil || !ran || rep.Removed != len(old) || rep.Told != 3 || rep.Passed != 0 {
			t.Fatalf("ran %v: %+v %v, want %d taken off and 3 told", ran, rep, err, len(old))
		}
		var record string
		if err := env.PG.QueryRow(`SELECT value FROM system_configs WHERE key = $1`, repair.Key).Scan(&record); err != nil || !strings.HasPrefix(record, "done: ") {
			t.Errorf("the repair's record: %q %v", record, err)
		}
		bot := principal()
		if bot == "" {
			t.Fatal("no system principal took them off")
		}
		if got := assigneeOf("moved"); got != people["ben"].String() {
			t.Errorf("the card an import made after the upgrade lost its assignee: %q", got)
		}
		for key, who := range old {
			now := read(key)
			if now.Assignee != nil {
				t.Errorf("%s is still assigned to %s", key, now.Assignee.ID)
			}
			var line *activity
			for i, a := range now.Activities {
				if a.Type == dgraphStruct.ACTIVITY_TYPE_ASSIGNEE {
					line = &now.Activities[i]
				}
			}
			if line == nil || line.By == nil || line.By.ID != bot || line.Prev != names[who] || line.Next != "" {
				t.Errorf("%s's history line: %+v, want %s taken off by the system principal", key, line, names[who])
			}
		}
		if got := read("old").Deleted; !strings.HasPrefix(got, "2026-10-01T09:00:00") {
			t.Errorf("the task in the trash left it: deleted at %q", got)
		}
		for key, who := range well {
			if got := assigneeOf(key); got != people[who].String() {
				t.Errorf("%s, which the assign rule still gives %s, is assigned to %q", key, who, got)
			}
		}
		if code := opens("ben", "kit"); code != http.StatusNotFound {
			t.Errorf("Ben opens the press kit after the upgrade: %d", code)
		}
		if code := opens("kim", "vault"); code != http.StatusNotFound {
			t.Errorf("Kim opens the restricted vault after the upgrade: %d", code)
		}
		if code := opens("gail", "ledger"); code != http.StatusOK {
			t.Errorf("Gail, let in, can't open the ledger she is given: %d", code)
		}

		want := []notice{
			{To: people["olu"].String(), Resource: "project:" + launch.String(), Data: map[string]string{"person": people["ben"].String(), "person_name": "Ben Ade", "tasks": "3"}},
			{To: people["olu"].String(), Resource: "project:" + launch.String(), Data: map[string]string{"person": people["kim"].String(), "person_name": "Kim Park", "tasks": "1"}},
			{To: people["wanda"].String(), Resource: "project:" + orphan.String(), Data: map[string]string{"person": people["ben"].String(), "person_name": "Ben Ade", "tasks": "1"}},
		}
		got := notices()
		if len(got) != len(want) {
			t.Fatalf("notices: %+v, want %+v", got, want)
		}
		byTo := map[string][]notice{}
		for _, n := range got {
			byTo[n.To+n.Resource+n.Data["person"]] = append(byTo[n.To+n.Resource+n.Data["person"]], n)
			if n.Actor != bot {
				t.Errorf("a notice's actor is %q, not the system principal", n.Actor)
			}
			for _, title := range []string{"Press kit", "Pricing", "Old venue", "Fix the form", "Loose end"} {
				if strings.Contains(strings.Join([]string{n.Data["person_name"], n.Data["tasks"], n.Resource}, " "), title) {
					t.Errorf("a notice names the task %q: %+v", title, n)
				}
			}
		}
		for _, w := range want {
			ns := byTo[w.To+w.Resource+w.Data["person"]]
			if len(ns) != 1 || len(ns[0].Data) != len(w.Data) {
				t.Errorf("for %s about %s: %+v, want one with %v", w.To, w.Resource, ns, w.Data)
				continue
			}
			for k, v := range w.Data {
				if ns[0].Data[k] != v {
					t.Errorf("for %s about %s: %s = %q, want %q", w.To, w.Resource, k, ns[0].Data[k], v)
				}
			}
		}

		// Nothing went to GitHub for the linked task: an ordinary unassign of
		// another linked task, started after it, queues its sync, so the
		// queue works here, and the repaired one's never came.
		maya := &dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: people["maya"].String(), UserName: names["maya"]}
		zero := time.Time{}
		ctrl := &dgraphStruct.DgraphTask{Uid: uids["ctrl"], Uuid: tasks["ctrl"].String(), DeletedAt: &zero, Assignee: maya}
		olu := &dgraphStruct.DgraphUser{Uid: uids["olu"], Uuid: people["olu"].String(), UserName: names["olu"]}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, tasks["ctrl"], nil, uids["maya"], uids["ctrl"], ctrl, olu); err != nil {
			t.Fatal(err)
		}
		queued := func(key string) int {
			var n int
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM github_sync_queue WHERE task_id = $1`, tasks[key]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		for deadline := time.Now().Add(10 * time.Second); queued("ctrl") == 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("an ordinary unassign of a linked task queued no sync")
			}
		}
		if n := queued("issue"); n != 0 {
			t.Errorf("the repair queued %d GitHub syncs for the linked task", n)
		}
	})

	t.Run("it runs once, and a second pass takes nobody off and tells nobody again", func(t *testing.T) {
		if _, ran, err := repair.Run(ctx); ran || err != nil {
			t.Fatalf("ran again on an install it was done on: %v %v", ran, err)
		}
		before := len(notices())
		lines := len(read("kit").Activities)
		again()
		rep, ran, err := repair.Run(ctx)
		if err != nil || !ran || rep.Removed != 0 || rep.Told != 0 {
			t.Fatalf("a second pass: %v %+v %v", ran, rep, err)
		}
		if after := len(notices()); after != before {
			t.Errorf("a second pass told the admins again: %d notices, had %d", after, before)
		}
		if after := len(read("kit").Activities); after != lines {
			t.Errorf("a second pass wrote the press kit's history again: %d lines, had %d", after, lines)
		}
	})
}
