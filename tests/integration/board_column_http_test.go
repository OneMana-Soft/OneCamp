//go:build integration

package integration_test

// DG4b through the handler: a project board read with the web's first page
// of 60, then each open column (and its custom status) walked by
// GET /project/boardColumn, is the whole board in its order; the walk keeps
// the board's filters; and the page says no to an outsider, a closed or
// unknown column, a card that left the column and a bad id.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestABoardColumnPagesThroughTheHandler(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC().Truncate(time.Second)
	ada, bob := uuid.New(), uuid.New()
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{ada, "ada"}, {bob, "bob"}} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, u.id, u.name+"@example.test", u.name, u.name); err != nil {
			t.Fatal(err)
		}
	}
	team, P := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Paging', $2)`, team, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Long board', $2, $3)`, P, team, ada); err != nil {
		t.Fatal(err)
	}
	ready := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO task_statuses (id, project_id, name, category, color, position) VALUES ($1, $2, 'Ready', 'todo', 'violet', 0)`, ready, P); err != nil {
		t.Fatal(err)
	}
	// Bob is in the workspace, not the project.
	base := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob", "user_deleted_at": live},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": P.String(), "project_name": "Long board", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ada"}}, "project_admins": []map[string]any{{"uid": "_:ada"}}},
	})

	// 170 of To do's own and 130 Ready, made in pairs in the same second (an
	// import's ties), a third of each high priority; 90 in progress; 5 done.
	var ids []uuid.UUID
	var batch []map[string]any
	add := func(status, custom string, n int) {
		for i := 0; i < n; i++ {
			id := uuid.New()
			m := map[string]any{"uid": "_:t" + id.String(), "dgraph.type": "Task", "task_uuid": id.String(), "task_name": fmt.Sprintf("%s %d", status, i),
				"task_status": status, "task_priority": []string{"low", "medium", "high"}[i%3], "task_created_at": now.Add(-time.Duration(len(ids)/2) * time.Minute).Format(time.RFC3339),
				"task_deleted_at": live, "task_start_date": live, "task_due_date": live, "task_project": map[string]any{"uid": base["P"]}}
			if custom != "" {
				m["task_custom_status"], m["task_custom_status_name"] = custom, "Ready"
			}
			batch = append(batch, m)
			ids = append(ids, id)
		}
	}
	add("todo", "", 170)
	add("todo", ready.String(), 130)
	add("inProgress", "", 90)
	add("done", "", 5)
	dg.Mutate(t, map[string]any{"uid": base["P"], "project_tasks": batch})
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by, created_at, updated_at) SELECT unnest($1::uuid[]), $2::uuid, $3::uuid, NOW(), NOW()`,
		pq.Array(ids), P, ada); err != nil {
		t.Fatal(err)
	}

	asAda := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: ada, EmailID: "ada@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base["ada"], Uuid: ada.String(), UserName: "ada"},
	}
	asBob := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: bob, EmailID: "bob@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base["bob"], Uuid: bob.String(), UserName: "bob"},
	}
	pp := map[string]string{"project_uuid": P.String()}
	board := "/project/taskListForKanban/" + P.String()
	column := "/project/boardColumn/" + P.String()
	partOf := func(c *dgraphStruct.DgraphTask) string {
		if c.CustomStatus != nil && *c.CustomStatus == ready.String() {
			return ready.String()
		}
		return c.Status
	}
	// The cards of one part, in the order a column shows them.
	part := func(cards []*dgraphStruct.DgraphTask, status string) (out []string) {
		for _, c := range cards {
			if partOf(c) == status {
				out = append(out, c.Uuid)
			}
		}
		return out
	}

	for _, filters := range []string{"", `[{"id":"task_priority","value":["high"]}]`} {
		q := ""
		if filters != "" {
			q = "&filters=" + url.QueryEscape(filters)
		}
		var whole, first dgraphStruct.DgraphProject
		if code := pastCall(t, projectController.GetProjectTaskListForKanban, "GET", board+"?openLimit=2000"+q, nil, asAda, pp, &whole); code != http.StatusOK {
			t.Fatalf("whole board: %d", code)
		}
		if code := pastCall(t, projectController.GetProjectTaskListForKanban, "GET", board+"?openLimit=60"+q, nil, asAda, pp, &first); code != http.StatusOK {
			t.Fatalf("first page: %d", code)
		}
		for _, c := range []struct {
			status string
			whole  []*dgraphStruct.DgraphTask
			first  []*dgraphStruct.DgraphTask
		}{
			{dgraphStruct.TASK_STATUS_TODO, whole.TasksTodo, first.TasksTodo},
			{ready.String(), whole.TasksTodo, first.TasksTodo},
			{dgraphStruct.TASK_STATUS_INPROGRESS, whole.TasksInProgresss, first.TasksInProgresss},
		} {
			want := part(c.whole, c.status)
			got := part(c.first, c.status)
			if len(got) != min(60, len(want)) {
				t.Fatalf("%s %q: the first answer has %d of %d", c.status, filters, len(got), len(want))
			}
			for more := len(got) < len(want); more; {
				var page projectBusiness.BoardColumnPage
				target := column + "?status=" + c.status + "&after=" + got[len(got)-1] + "&limit=60" + q
				if code := pastCall(t, projectController.GetProjectBoardColumn, "GET", target, nil, asAda, pp, &page); code != http.StatusOK {
					t.Fatalf("%s after %d: %d", c.status, len(got), code)
				}
				if page.Total != len(want) || len(page.Cards) == 0 || len(page.Cards) > 60 {
					t.Fatalf("%s %q: a page of %d, total %d, want pages of at most 60 and %d in all", c.status, filters, len(page.Cards), page.Total, len(want))
				}
				got = append(got, part(page.Cards, c.status)...)
				more = page.More
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s %q: the walk (%d cards) is not the whole column (%d) in its order", c.status, filters, len(got), len(want))
			}
		}
	}

	// limit past the most is the most.
	var big projectBusiness.BoardColumnPage
	if code := pastCall(t, projectController.GetProjectBoardColumn, "GET", column+"?status=todo&limit=5000", nil, asAda, pp, &big); code != http.StatusOK || len(big.Cards) != 170 || big.More {
		t.Fatalf("limit 5000: %d, %d cards, more %v", code, len(big.Cards), big.More)
	}

	for _, c := range []struct {
		name string
		user userModels.UserInfo
		q    string
		code int
	}{
		{"an outsider", asBob, "?status=todo", http.StatusForbidden},
		{"a closed column", asAda, "?status=done", http.StatusBadRequest},
		{"no such column", asAda, "?status=nonsense", http.StatusBadRequest},
		{"no status", asAda, "", http.StatusBadRequest},
		{"after that isn't an id", asAda, "?status=todo&after=0x1", http.StatusBadRequest},
		{"after a card not in the column", asAda, "?status=todo&after=" + uuid.NewString(), http.StatusConflict},
	} {
		var page projectBusiness.BoardColumnPage
		if code := pastCall(t, projectController.GetProjectBoardColumn, "GET", column+c.q, nil, c.user, pp, &page); code != c.code {
			t.Errorf("%s: %d, want %d", c.name, code, c.code)
		}
	}
	// An outsider is told no before anything is read, whatever the page asks.
	var leak projectBusiness.BoardColumnPage
	if code := pastCall(t, projectController.GetProjectBoardColumn, "GET", column+"?status="+ready.String(), nil, asBob, pp, &leak); code != http.StatusForbidden || len(leak.Cards) != 0 {
		t.Fatalf("an outsider's Ready page: %d, %d cards", code, len(leak.Cards))
	}
}
