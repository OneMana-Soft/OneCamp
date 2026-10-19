//go:build integration
// +build integration

package integration_test

// Docs C1, connected work, through its handlers against real Postgres 12,
// Dgraph and Redis (search answered by a stub): a chip shows each thing only
// to whoever may open it (a doc as the Access Resolver decides, a task and a
// project by the task view's rule), and a doc is linked to a task only by
// someone it may be named to (ReadableTitles); the "@" list learns who can't
// open a doc; a to-do becomes a task only for someone who may add tasks to
// the project, tied to its doc, and a tick in the doc changes the task only
// for someone who may change it; the
// ties keep, list and go; and the ties' migration goes down and up again.
//
// Run: go test -tags=integration ./tests/integration/ -run TestConnectedWork -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	linkModels "github.com/akashc777/OneCamp/models/postgres/EntityLink"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// tiesMigration finds the ties' migration by its name, so a renumbering at
// merge moves this test with it.
func tiesMigration(t *testing.T) (dir string, version uint) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	matches, err := filepath.Glob(filepath.Join(dir, "*_doc_task_links.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the ties' migration: %v %v", matches, err)
	}
	n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return dir, uint(n)
}

func TestConnectedWork(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	// Olu runs project P (its admin) and owns the docs; Gus is in P and may
	// edit Plan; Cal may comment on Plan; Zed, Ava and Ben are in neither.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"olu", "gus", "zed", "cal", "ava", "ben"} {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	team, project, task := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Launch', $2)`, team, people["olu"]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'P', $2, $3)`, project, team, people["olu"]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, people["olu"]); err != nil {
		t.Fatal(err)
	}
	// Gus is in the project and only views it (Work Access, A1): members
	// change tasks by default, so the cases below need someone who may not.
	if _, err := env.PG.Exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, project, people["gus"]); err != nil {
		t.Fatal(err)
	}
	zero := "0001-01-01T00:00:00Z"
	nodes = append(nodes,
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Launch", "team_deleted_at": zero,
			"team_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:gus"}}, "team_admins": []map[string]any{{"uid": "_:olu"}}},
		map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "P", "project_deleted_at": zero,
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:gus"}},
			"project_admins": []map[string]any{{"uid": "_:olu"}}},
		map[string]any{"uid": "_:task", "dgraph.type": "Task", "task_uuid": task.String(), "task_name": "Record the film", "task_status": "todo",
			"task_deleted_at": zero, "task_project": map[string]any{"uid": "_:p"}, "task_created_by": map[string]any{"uid": "_:olu"}},
	)
	uids := dg.Mutate(t, nodes)

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		return u
	}
	call := func(who, target string, handler http.HandlerFunc, body any) answer {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", target, bytes.NewReader(raw))
		c := context.WithValue(ctx, helpers.UserInfoContextKey, as(who))
		req = req.WithContext(context.WithValue(c, chi.RouteCtxKey, chi.NewRouteContext()))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	create := func(title string) string {
		t.Helper()
		a := call("olu", "/doc/createDoc", docController.CreateDocInTree, map[string]any{"doc_title": title, "doc_private": true})
		if a.code != 200 {
			t.Fatalf("making %q: %d %v", title, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
	}
	plan, secret := create("Plan"), create("Secret")
	if a := call("olu", "/doc/access/update", docController.UpdateDocAccess, map[string]any{"doc_uuid": plan, "adds": []any{
		map[string]any{"kind": "user", "uuid": people["gus"].String(), "level": "edit"},
		map[string]any{"kind": "user", "uuid": people["cal"].String(), "level": "comment"},
	}}); a.code != 200 {
		t.Fatalf("sharing Plan with Gus and Cal: %d %v", a.code, a.body)
	}

	// entities is what each chip shows who, by kind and id.
	entities := func(who string, refs ...[2]string) map[string]map[string]any {
		t.Helper()
		var in []map[string]string
		for _, r := range refs {
			in = append(in, map[string]string{"kind": r[0], "id": r[1]})
		}
		a := call(who, "/entities", entityLinkController.ReadEntities, map[string]any{"refs": in})
		if a.code != 200 {
			t.Fatalf("%s reading chips: %d %v", who, a.code, a.body)
		}
		out := map[string]map[string]any{}
		for _, e := range list(a.body["data"]) {
			m := e.(map[string]any)
			out[str(m["kind"])+":"+str(m["id"])] = m
		}
		return out
	}
	everything := [][2]string{{"doc", plan}, {"doc", secret}, {"task", task.String()}, {"project", project.String()}, {"person", people["olu"].String()}}

	t.Run("a chip shows each thing only to whoever may open it", func(t *testing.T) {
		gus := entities("gus", everything...)
		if e := gus["doc:"+plan]; str(e["access"]) != "ok" || str(e["data"].(map[string]any)["title"]) != "Plan" || str(e["topic"]) != helpers.GetMqttTopicForDoc(plan) {
			t.Errorf("Plan for Gus: %v", e)
		}
		if e := gus["doc:"+secret]; str(e["access"]) != "none" || e["data"] != nil || e["topic"] != nil {
			t.Errorf("Secret told Gus something: %v", e)
		}
		if e := gus["task:"+task.String()]; str(e["access"]) != "ok" || e["data"].(map[string]any)["can_edit"] != false ||
			str(e["data"].(map[string]any)["name"]) != "Record the film" || str(e["topic"]) != helpers.GetMqttTopicForProjectMessage(project.String()) {
			t.Errorf("the task for Gus, a viewer: %v", e)
		}
		if e := gus["project:"+project.String()]; str(e["access"]) != "ok" || e["data"].(map[string]any)["can_edit"] != false {
			t.Errorf("the project for Gus: %v", e)
		}
		if e := gus["person:"+people["olu"].String()]; str(e["access"]) != "ok" || str(e["data"].(map[string]any)["name"]) != "olu" {
			t.Errorf("Olu for Gus: %v", e)
		}
		olu := entities("olu", everything...)
		if olu["task:"+task.String()]["data"].(map[string]any)["can_edit"] != true || olu["project:"+project.String()]["data"].(map[string]any)["can_edit"] != true {
			t.Errorf("Olu runs the project: %v %v", olu["task:"+task.String()], olu["project:"+project.String()])
		}
		zed := entities("zed", everything...)
		for _, key := range []string{"doc:" + plan, "doc:" + secret, "task:" + task.String(), "project:" + project.String()} {
			if e := zed[key]; str(e["access"]) != "none" || e["data"] != nil {
				t.Errorf("%s told Zed something: %v", key, e)
			}
		}
	})

	t.Run("a doc is linked to a task only by someone it may be named to", func(t *testing.T) {
		may := func(who, doc string) bool {
			ok, err := entityLinkBusiness.CanReadRef(ctx, entityLinkBusiness.RefDoc, doc, uids[who], people[who].String())
			if err != nil {
				t.Fatalf("%s and %s: %v", who, doc, err)
			}
			return ok
		}
		if !may("gus", plan) || may("gus", secret) || may("zed", plan) || !may("olu", secret) {
			t.Errorf("Gus: Plan %v, Secret %v; Zed: Plan %v; Olu: Secret %v", may("gus", plan), may("gus", secret), may("zed", plan), may("olu", secret))
		}
	})

	// mentionAccess is what the "@" list in doc learns, writing as who, about Gus and Zed.
	mentionAccess := func(who, doc string) answer {
		return call(who, "/doc/mentionAccess", entityLinkController.MentionAccess, map[string]any{
			"doc_uuid": doc, "user_uuids": []string{people["gus"].String(), people["zed"].String()},
		})
	}

	t.Run("the @ list learns who can't open the doc, and who may share it", func(t *testing.T) {
		if a := mentionAccess("olu", plan); a.code != 200 || a.data()["can_share"] != true || fmt.Sprint(list(a.data()["cant_open"])) != fmt.Sprint([]any{people["zed"].String()}) {
			t.Errorf("Olu: %d %v", a.code, a.body)
		}
		// Sharing with someone mentioned is an invite, and an editor may invite.
		if a := mentionAccess("gus", plan); a.code != 200 || a.data()["can_share"] != true {
			t.Errorf("Gus may edit Plan, so may share it with someone he mentions: %d %v", a.code, a.body)
		}
		// A commenter writes in no doc's body: the list asks nothing for him.
		if a := mentionAccess("cal", plan); a.code != 404 {
			t.Errorf("Cal, who may only comment, was offered sharing: %d %v", a.code, a.body)
		}
		if a := mentionAccess("zed", plan); a.code != 404 {
			t.Errorf("Zed, who can't open Plan, was told who can: %d %v", a.code, a.body)
		}
	})

	var made string
	t.Run("a to-do becomes a task only for someone who may add tasks", func(t *testing.T) {
		in := map[string]any{"doc_uuid": plan, "project_uuid": project.String(), "title": "Write the launch post"}
		if a := call("gus", "/doc/todoTask", entityLinkController.TodoToTask, in); a.code != 403 {
			t.Fatalf("Gus, a viewer, made a task: %d %v", a.code, a.body)
		}
		if a := call("zed", "/doc/todoTask", entityLinkController.TodoToTask, in); a.code != 403 {
			t.Fatalf("Zed, who can't edit Plan, made a task: %d %v", a.code, a.body)
		}
		a := call("olu", "/doc/todoTask", entityLinkController.TodoToTask, in)
		if a.code != 200 {
			t.Fatalf("Olu: %d %v", a.code, a.body)
		}
		made = str(a.data()["task_uuid"])
		chip := entities("olu", [2]string{"task", made})["task:"+made]
		if str(chip["access"]) != "ok" || str(chip["data"].(map[string]any)["name"]) != "Write the launch post" || str(chip["data"].(map[string]any)["status"]) != "todo" {
			t.Fatalf("the new task: %v", chip)
		}
		if tied, err := linkModels.HasDocTask(ctx, uuid.MustParse(plan), uuid.MustParse(made)); err != nil || !tied {
			t.Fatalf("Plan doesn't hold the task: %v %v", tied, err)
		}
	})

	t.Run("a tick in the doc changes the task only for whoever may change it", func(t *testing.T) {
		tick := func(who string, done bool) answer {
			return call(who, "/doc/todoTask/check", entityLinkController.CheckTodo, map[string]any{"doc_uuid": plan, "task_uuid": made, "done": done})
		}
		if a := tick("gus", true); a.code != 403 {
			t.Fatalf("Gus changed the task from the doc: %d %v", a.code, a.body)
		}
		if a := tick("olu", true); a.code != 200 {
			t.Fatalf("Olu: %d %v", a.code, a.body)
		}
		if s := str(entities("olu", [2]string{"task", made})["task:"+made]["data"].(map[string]any)["status"]); s != "done" {
			t.Fatalf("the task is %q after the tick", s)
		}
		// The task the doc doesn't hold is not the doc's to change.
		if a := call("olu", "/doc/todoTask/check", entityLinkController.CheckTodo, map[string]any{"doc_uuid": secret, "task_uuid": made, "done": false}); a.code != 404 {
			t.Fatalf("a doc that doesn't hold the task changed it: %d %v", a.code, a.body)
		}
	})

	t.Run("the ties keep, list and go", func(t *testing.T) {
		doc, other := uuid.MustParse(secret), uuid.MustParse(made)
		for i := 0; i < 2; i++ {
			if err := linkModels.AddDocTask(ctx, doc, other, people["olu"]); err != nil {
				t.Fatal(err)
			}
		}
		docs, err := linkModels.DocsForTask(ctx, other)
		if err != nil || len(docs) != 2 || docs[0] != uuid.MustParse(plan) || docs[1] != doc {
			t.Fatalf("the docs holding the task: %v %v", docs, err)
		}
		if err := linkModels.RemoveDocTask(ctx, doc, other); err != nil {
			t.Fatal(err)
		}
		docs, err = linkModels.DocsForTask(ctx, other)
		if err != nil || len(docs) != 1 || docs[0] != uuid.MustParse(plan) {
			t.Fatalf("after Secret let the task go: %v %v", docs, err)
		}
	})

	t.Run("sharing with someone mentioned is an invite, at the sharer's own level or below", func(t *testing.T) {
		invite := func(who, whom, level string) answer {
			return call(who, "/doc/access/invite", docController.InviteToDoc, map[string]any{"doc_uuid": plan, "adds": []any{
				map[string]any{"kind": "user", "uuid": people[whom].String(), "level": level},
			}})
		}
		// Gus may edit Plan: he shares it to view and to edit, his own level, never above.
		if a := invite("gus", "zed", "view"); a.code != 200 {
			t.Errorf("Gus sharing Plan with Zed to view: %d %v", a.code, a.body)
		}
		if a := invite("gus", "ava", "edit"); a.code != 200 {
			t.Errorf("Gus sharing Plan with Ava to edit: %d %v", a.code, a.body)
		}
		if a := invite("gus", "ben", "full"); a.code != 403 || str(a.body["code"]) != "above_your_level" {
			t.Errorf("Gus gave Ben more access than he has: %d %v", a.code, a.body)
		}
		// Cal may only comment: he is offered nothing, and refused when he asks anyway.
		if a := invite("cal", "ben", "view"); a.code != 403 || str(a.body["code"]) != "forbidden" {
			t.Errorf("Cal, a commenter, shared Plan: %d %v", a.code, a.body)
		}
		// Full access is as it was: Olu shares at any level.
		if a := invite("olu", "ben", "full"); a.code != 200 {
			t.Errorf("Olu sharing Plan with Ben, full access: %d %v", a.code, a.body)
		}
		// Each has what they were given, and no more: Ava edits (so may share in turn), Zed only reads.
		if a := mentionAccess("ava", plan); a.code != 200 || a.data()["can_share"] != true {
			t.Errorf("Ava, given edit: %d %v", a.code, a.body)
		}
		if a := mentionAccess("zed", plan); a.code != 404 {
			t.Errorf("Zed, given view, writes in Plan: %d %v", a.code, a.body)
		}
		if e := entities("zed", [2]string{"doc", plan})["doc:"+plan]; str(e["access"]) != "ok" {
			t.Errorf("Zed can't read Plan after Gus shared it: %v", e)
		}
	})

	t.Run("the ties' migration goes down and up again", func(t *testing.T) {
		dir, version := tiesMigration(t)
		m, err := migrate.New("file://"+dir, env.DSN)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		for v, _, _ := m.Version(); v >= version; v, _, _ = m.Version() {
			if err := m.Steps(-1); err != nil {
				t.Fatal(err)
			}
		}
		var exists bool
		_ = env.PG.QueryRow(`SELECT to_regclass('doc_task_links') IS NOT NULL`).Scan(&exists)
		if exists {
			t.Fatal("doc_task_links is still there after down")
		}
		if err := m.Up(); err != nil {
			t.Fatal(err)
		}
		_ = env.PG.QueryRow(`SELECT to_regclass('doc_task_links') IS NOT NULL`).Scan(&exists)
		if !exists {
			t.Fatal("doc_task_links didn't come back")
		}
	})
}
