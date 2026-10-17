//go:build integration

package integration_test

// A task list sorted by each sort it offers, through the real handlers. The
// lists' Sort menu sent task_created_date for Created, which no task has; the
// handlers passed it to the graph as it came, and the graph refused the whole
// list ("Cannot sort by unknown attribute"), so Created emptied the list with
// an error, on the project's list and on My Tasks alike. Saved views and
// shared links keep that key.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTaskListsSortByEverySortTheyOffer(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = nil })

	const live = "0001-01-01T00:00:00Z"
	ada, team, P := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ada@example.test', 'ada', 'Ada', NOW(), NOW())`, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Sorts', $2)`, team, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Launch', $2, $3)`, P, team, ada); err != nil {
		t.Fatal(err)
	}
	base := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": P.String(), "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ada"}}, "project_admins": []map[string]any{{"uid": "_:ada"}}},
	})
	// Three tasks whose dates put them in a different order by each sort:
	// made A, B, C; starting B, C, A; due C, A, B.
	day := func(d int) string { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC).Format(time.RFC3339) }
	dates := map[string][3]int{"A": {1, 3, 2}, "B": {2, 1, 3}, "C": {3, 2, 1}} // created, start, due
	var tasks []map[string]any
	for _, name := range []string{"A", "B", "C"} {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by, created_at, updated_at) VALUES ($1, $2, $3, NOW(), NOW())`, id, P, ada); err != nil {
			t.Fatal(err)
		}
		d := dates[name]
		tasks = append(tasks, map[string]any{"uid": "_:" + name, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": name,
			"task_status": "todo", "task_priority": "medium", "task_deleted_at": live,
			"task_created_at": day(d[0]), "task_start_date": day(d[1]), "task_due_date": day(d[2]),
			"task_project":  map[string]any{"uid": base["P"]},
			"task_assignee": map[string]any{"uid": base["ada"], "user_tasks": []map[string]any{{"uid": "_:" + name}}}})
	}
	dg.Mutate(t, map[string]any{"uid": base["P"], "project_tasks": tasks})

	me := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: ada, EmailID: "ada@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base["ada"], Uuid: ada.String(), UserName: "ada"},
	}
	names := func(tasks []*dgraphStruct.DgraphTask) string {
		var out []string
		for _, t := range tasks {
			out = append(out, t.Name)
		}
		return strings.Join(out, "")
	}
	reverse := func(s string) string {
		r := []byte(s)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r)
	}
	// Every sort the lists offer: the column headers' keys and the Sort
	// menu's, Created by either.
	for key, ascending := range map[string]string{
		"task_start_date":   "BCA",
		"task_due_date":     "CAB",
		"task_created_at":   "ABC",
		"task_created_date": "ABC",
	} {
		for _, desc := range []bool{false, true} {
			want := ascending
			if desc {
				want = reverse(ascending)
			}
			sorting := url.QueryEscape(fmt.Sprintf(`[{"id":%q,"desc":%v}]`, key, desc))
			for _, paging := range []string{"pageSize=10&pageIndex=0", "getAll=true"} {
				var list dgraphStruct.DgraphProject
				target := "/project/taskList/" + P.String() + "?" + paging + "&sorting=" + sorting
				if code := pastCall(t, projectController.GetProjectTaskList, "GET", target, nil, me, map[string]string{"project_uuid": P.String()}, &list); code != http.StatusOK {
					t.Errorf("the project's list sorted by %s (desc %v, %s): %d", key, desc, paging, code)
				} else if got := names(list.Tasks); got != want {
					t.Errorf("the project's list sorted by %s (desc %v, %s): %s, want %s", key, desc, paging, got, want)
				}
				var mine dgraphStruct.DgraphUser
				target = "/user/assignedTaskList?" + paging + "&sorting=" + sorting
				if code := pastCall(t, userController.GetDgraphUserTaskList, "GET", target, nil, me, nil, &mine); code != http.StatusOK {
					t.Errorf("My Tasks sorted by %s (desc %v, %s): %d", key, desc, paging, code)
				} else if got := names(mine.Tasks); got != want {
					t.Errorf("My Tasks sorted by %s (desc %v, %s): %s, want %s", key, desc, paging, got, want)
				}
			}
		}
	}
	// A key the lists don't offer reaches no graph: newest first.
	var list dgraphStruct.DgraphProject
	odd := url.QueryEscape(`[{"id":"task_rank","desc":false}]`)
	if code := pastCall(t, projectController.GetProjectTaskList, "GET", "/project/taskList/"+P.String()+"?pageSize=10&pageIndex=0&sorting="+odd, nil, me, map[string]string{"project_uuid": P.String()}, &list); code != http.StatusOK || names(list.Tasks) != "CBA" {
		t.Errorf("a sort the list doesn't offer: %d, %s; want newest first, CBA", code, names(list.Tasks))
	}
}
