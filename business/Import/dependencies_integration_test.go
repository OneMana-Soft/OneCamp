//go:build integration

package business

// I1: a source's "blocked by" links land as the tasks' dependencies once every
// task has come across; a blocker that didn't come across is said in the
// import's log. Against Postgres 12 with every migration and Dgraph.
// Run: PKGS=./business/Import integ_rel.sh <wt> <log> 'TestImportedDependencies'

import (
	"context"
	"encoding/json"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestImportedDependencies(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	job := &importModels.Job{Id: uuid.New(), Provider: "jira", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	ana, project := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ana@deps.test', 'ana', 'Ana', NOW(), NOW())`, ana)
	tasks := map[string]uuid.UUID{"ENG-1": uuid.New(), "ENG-2": uuid.New(), "ENG-3": uuid.New()}
	live := "0001-01-01T00:00:00Z"
	nodes := []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana", "user_deleted_at": live},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Imported", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ana"}}, "project_admins": []map[string]any{{"uid": "_:ana"}}},
	}
	for key, id := range tasks {
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": key,
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:p"}})
		exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, NULL, $2)`, id, ana)
	}
	uids := dg.Mutate(t, nodes)

	// ENG-3 waits on ENG-1 and ENG-2 (ss, two days on); ENG-2 waits on one
	// that wasn't imported.
	mapTask := func(src string, deps []importProvider.SourceDependency) {
		t.Helper()
		md := taskMapMetadata(&importProvider.SourceTask{SourceID: src, Name: src, Dependencies: deps}, "todo", "medium")
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityTask, src, tasks[src], nil, mustMarshal(md), true); err != nil {
			t.Fatal(err)
		}
	}
	mapTask("ENG-1", nil)
	mapTask("ENG-2", []importProvider.SourceDependency{{BlockerSourceID: "ENG-404"}})
	mapTask("ENG-3", []importProvider.SourceDependency{{BlockerSourceID: "ENG-1"}, {BlockerSourceID: "ENG-2", Kind: "ss", LagDays: 2}})

	user := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["ana"], Uuid: ana.String(), UserName: "ana"}}
	user.UserPostgresInfo.Id = ana
	if err := runDependenciesStage(ctx, job, user); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Q []struct {
			B []struct {
				Name string `json:"task_name"`
				Kind string `json:"task_blocked_by|kind"`
				Lag  int    `json:"task_blocked_by|lag"`
			} `json:"task_blocked_by"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(task_uuid, "`+tasks["ENG-3"].String()+`")) { task_blocked_by @facets(kind, lag) { task_name } } }`), &got); err != nil || len(got.Q) != 1 {
		t.Fatalf("ENG-3: %+v (%v)", got, err)
	}
	waits := map[string]string{}
	for _, b := range got.Q[0].B {
		waits[b.Name] = b.Kind
	}
	if len(waits) != 2 || waits["ENG-2"] != "ss" {
		t.Fatalf("ENG-3 waits on %+v, want ENG-1 and ENG-2 (start to start)", got.Q[0].B)
	}

	var unresolved int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM import_errors WHERE import_id = $1 AND code = 'DEPENDENCY_UNRESOLVED' AND source_id = 'ENG-2'`, job.Id).Scan(&unresolved); err != nil || unresolved != 1 {
		t.Fatalf("the unresolved blocker said %d times (%v), want once", unresolved, err)
	}
}
