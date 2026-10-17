//go:build integration

package integration_test

// PROJ-BUG-8: Dgraph returns at most 1,000 results from a sorted block that
// names no first, and says nothing. Board columns, My Tasks, "show all" lists,
// a task's history and the column a reorder renumbers were all read that way,
// so an imported backlog lost cards past the 1,000th without a word. These
// seed more than 1,000 tasks in one column (and more than 1,000 history lines
// on one task), call the real handlers, and check that every card can be
// reached and that the counts are right.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// call runs a handler as user and decodes the answer's data into out.
func pastCall(t *testing.T, h http.HandlerFunc, method, target string, body any, user userModels.UserInfo, params map[string]string, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rdr)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, helpers.UserInfoContextKey, user)
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	if out != nil && rec.Code == http.StatusOK {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
	}
	return rec.Code
}

func uuidsOf(tasks []*dgraphStruct.DgraphTask) map[string]bool {
	out := map[string]bool{}
	for _, t := range tasks {
		out[t.Uuid] = true
	}
	return out
}

func TestBoardsListsAndHistoryPastAThousand(t *testing.T) {
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
	rfc := func(tm time.Time) string { return tm.UTC().Format(time.RFC3339) }
	now := time.Now().UTC().Truncate(time.Second)

	ada := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ada@example.test', 'ada', 'Ada', NOW(), NOW())`, ada); err != nil {
		t.Fatal(err)
	}
	team, P := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Imports', $2)`, team, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Jira backlog', $2, $3)`, P, team, ada); err != nil {
		t.Fatal(err)
	}
	// A custom status under To do: a column of its own on the board.
	ready := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO task_statuses (id, project_id, name, category, color, position) VALUES ($1, $2, 'Ready', 'todo', 'violet', 0)`, ready, P); err != nil {
		t.Fatal(err)
	}
	// Bob is in the workspace but not the project.
	bob := uuid.New()
	base := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob", "user_deleted_at": live},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": P.String(), "project_name": "Jira backlog", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ada"}}, "project_admins": []map[string]any{{"uid": "_:ada"}}},
	})

	// To do: 1,200 of its own and 1,050 Ready, in pairs made in the same
	// second (an import's ties); 1,300 done; 20 in progress. Ada has every
	// task of her own To do and all the done ones.
	type seeded struct {
		id      string
		status  string
		custom  string
		created time.Time
	}
	var all []seeded
	var ids []uuid.UUID
	uidOf := map[string]string{}
	add := func(status, custom string, n int, assigned bool) {
		for b := 0; b < n; b += 500 {
			var batch []map[string]any
			for i := b; i < min(b+500, n); i++ {
				id := uuid.New()
				created := now.Add(-time.Duration(len(all)/2) * time.Minute)
				m := map[string]any{"uid": "_:t" + id.String(), "dgraph.type": "Task", "task_uuid": id.String(), "task_name": fmt.Sprintf("%s %d", status, i),
					"task_status": status, "task_priority": []string{"low", "medium", "high"}[i%3], "task_created_at": rfc(created), "task_deleted_at": live,
					"task_start_date": live, "task_due_date": live, "task_project": map[string]any{"uid": base["P"]}}
				if custom != "" {
					m["task_custom_status"], m["task_custom_status_name"] = custom, "Ready"
				}
				if assigned {
					m["task_assignee"] = map[string]any{"uid": base["ada"], "user_tasks": []map[string]any{{"uid": "_:t" + id.String()}}}
				}
				batch = append(batch, m)
				all = append(all, seeded{id: id.String(), status: status, custom: custom, created: created})
				ids = append(ids, id)
			}
			for k, v := range dg.Mutate(t, map[string]any{"uid": base["P"], "project_tasks": batch}) {
				uidOf[strings.TrimPrefix(k, "t")] = v
			}
		}
	}
	add("todo", "", 1200, true)
	add("todo", ready.String(), 1050, false)
	add("done", "", 1300, true)
	add("inProgress", "", 20, false)
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by, created_at, updated_at) SELECT unnest($1::uuid[]), $2::uuid, $3::uuid, NOW(), NOW()`,
		pq.Array(ids), P, ada); err != nil {
		t.Fatal(err)
	}
	ownTodo, readyTodo := map[string]bool{}, map[string]bool{}
	var oldest seeded
	for _, s := range all {
		switch {
		case s.status == "todo" && s.custom == "":
			ownTodo[s.id] = true
			oldest = s
		case s.status == "todo":
			readyTodo[s.id] = true
		}
	}
	// The oldest of To do's own was dragged to the top long ago: it is the
	// column's first card, though 1,199 of its own were made after it.
	dg.Mutate(t, map[string]any{"uid": uidOf[oldest.id], "task_rank": -1e15})

	ada1 := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: ada, EmailID: "ada@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base["ada"], Uuid: ada.String(), UserName: "ada"},
	}
	pp := map[string]string{"project_uuid": P.String()}
	split := func(col []*dgraphStruct.DgraphTask) (own, rdy []*dgraphStruct.DgraphTask) {
		for _, t := range col {
			if t.CustomStatus != nil && *t.CustomStatus == ready.String() {
				rdy = append(rdy, t)
			} else {
				own = append(own, t)
			}
		}
		return
	}
	sameSet := func(name string, got []*dgraphStruct.DgraphTask, want map[string]bool) {
		t.Helper()
		g := uuidsOf(got)
		if len(got) != len(want) || len(g) != len(want) {
			t.Fatalf("%s: %d cards (%d different), want %d", name, len(got), len(g), len(want))
		}
		for id := range want {
			if !g[id] {
				t.Fatalf("%s: task %s missing", name, id)
			}
		}
	}

	// The board, as it first loads: each To do column's first 1,000 in board
	// order, and every column's count.
	var board dgraphStruct.DgraphProject
	if code := pastCall(t, projectController.GetProjectTaskListForKanban, "GET", "/project/taskListForKanban/"+P.String(), nil, ada1, pp, &board); code != http.StatusOK {
		t.Fatalf("board: %d", code)
	}
	own, rdy := split(board.TasksTodo)
	if len(own) != dgraphStruct.BoardOpenLimit || len(rdy) != dgraphStruct.BoardOpenLimit {
		t.Fatalf("first load: To do %d and Ready %d cards, want %d of each", len(own), len(rdy), dgraphStruct.BoardOpenLimit)
	}
	if own[0].Uuid != oldest.id {
		t.Fatalf("the card dragged to the top is not first: %s", own[0].Name)
	}
	wantCounts := map[string]int{"todo": 1200, ready.String(): 1050, "done": 1300, "inProgress": 20, "backlog": 0, "inReview": 0, "canceled": 0}
	for k, v := range wantCounts {
		if got, ok := board.ColumnCounts[k]; !ok || got != v {
			t.Errorf("project_column_counts[%s] = %d (present %v), want %d", k, got, ok, v)
		}
	}
	if len(board.TasksDone) != dgraphStruct.BoardClosedLimit || board.TasksDoneCount != 1300 || len(board.TasksInProgresss) != 20 {
		t.Fatalf("done %d of %d, in progress %d", len(board.TasksDone), board.TasksDoneCount, len(board.TasksInProgresss))
	}

	// Show more: every card is reachable, and the first page is where the
	// bigger one starts.
	var more dgraphStruct.DgraphProject
	pastCall(t, projectController.GetProjectTaskListForKanban, "GET", "/project/taskListForKanban/"+P.String()+"?openLimit=2000", nil, ada1, pp, &more)
	allOwn, allReady := split(more.TasksTodo)
	sameSet("To do, openLimit 2000", allOwn, ownTodo)
	sameSet("Ready, openLimit 2000", allReady, readyTodo)
	for i := range own {
		if own[i].Uuid != allOwn[i].Uuid {
			t.Fatalf("card %d of the first page is %s, of the bigger page %s", i, own[i].Name, allOwn[i].Name)
		}
	}
	var garbage dgraphStruct.DgraphProject
	pastCall(t, projectController.GetProjectTaskListForKanban, "GET", "/project/taskListForKanban/"+P.String()+"?openLimit=lots", nil, ada1, pp, &garbage)
	if len(garbage.TasksTodo) != 2*dgraphStruct.BoardOpenLimit {
		t.Fatalf("a bad openLimit gave %d To do cards, want the default", len(garbage.TasksTodo))
	}

	// Server readers that want everything (update drafts, the time report).
	everything, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, P.String(), base["ada"], "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(everything.TasksTodo) != 2250 || len(everything.TasksDone) != 1300 {
		t.Fatalf("every card: To do %d, done %d; want 2250 and 1300", len(everything.TasksTodo), len(everything.TasksDone))
	}

	// My Tasks: the same, across her projects.
	var mine dgraphStruct.DgraphUser
	pastCall(t, userController.GetDgraphUserTaskListForKanban, "GET", "/user/assignedTaskListForKanban", nil, ada1, nil, &mine)
	if len(mine.TasksTodo) != dgraphStruct.BoardOpenLimit || mine.ColumnCounts["todo"] != 1200 || mine.ColumnCounts["done"] != 1300 || len(mine.ColumnCounts) != 6 {
		t.Fatalf("My Tasks: %d To do cards, counts %v", len(mine.TasksTodo), mine.ColumnCounts)
	}
	var mineMore dgraphStruct.DgraphUser
	pastCall(t, userController.GetDgraphUserTaskListForKanban, "GET", "/user/assignedTaskListForKanban?openLimit=2000", nil, ada1, nil, &mineMore)
	sameSet("My Tasks To do, openLimit 2000", mineMore.TasksTodo, ownTodo)

	// "Show all" lists: every task, whatever they are sorted by, ties and all.
	every := map[string]bool{}
	for _, s := range all {
		every[s.id] = true
	}
	byPriority := url.QueryEscape(`[{"id":"task_priority","desc":false}]`)
	for _, q := range []string{"getAll=true", "getAll=true&sorting=" + byPriority, "pageSize=0&pageIndex=0"} {
		var list dgraphStruct.DgraphProject
		if code := pastCall(t, projectController.GetProjectTaskList, "GET", "/project/taskList/"+P.String()+"?"+q, nil, ada1, pp, &list); code != http.StatusOK {
			t.Fatalf("list %s: %d", q, code)
		}
		if list.TaskCount != uint64(len(all)) {
			t.Fatalf("list %s: count %d, want %d", q, list.TaskCount, len(all))
		}
		sameSet("list "+q, list.Tasks, every)
	}
	// Pages of a list sorted by a key most tasks share still meet.
	var paged []*dgraphStruct.DgraphTask
	for page := 0; page*1000 < len(all); page++ {
		var list dgraphStruct.DgraphProject
		pastCall(t, projectController.GetProjectTaskList, "GET", fmt.Sprintf("/project/taskList/%s?pageSize=1000&pageIndex=%d&sorting=%s", P, page, byPriority), nil, ada1, pp, &list)
		paged = append(paged, list.Tasks...)
	}
	sameSet("pages sorted by priority", paged, every)
	var myList dgraphStruct.DgraphUser
	pastCall(t, userController.GetDgraphUserTaskList, "GET", "/user/assignedTaskList?getAll=true", nil, ada1, nil, &myList)
	if len(myList.Tasks) != 2500 || len(uuidsOf(myList.Tasks)) != 2500 || myList.TaskCount != 2500 {
		t.Fatalf("My Tasks list: %d tasks (%d different), count %d; want 2500", len(myList.Tasks), len(uuidsOf(myList.Tasks)), myList.TaskCount)
	}

	// A task with 1,100 history lines: the panel has the newest 1,000, oldest
	// first, and the rest page back from the oldest it shows.
	hid := all[0].id
	var lines []map[string]any
	var lineIDs []string
	start := now.Add(-2000 * time.Hour)
	for i := 0; i < 1100; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		if i >= 47 && i < 53 { // a save that wrote six lines at once, across a page's edge
			at = start.Add(47 * time.Hour)
		}
		id := uuid.NewString()
		lineIDs = append(lineIDs, id)
		lines = append(lines, map[string]any{"dgraph.type": "Activity", "activity_uuid": id, "activity_type": dgraphStruct.ACTIVITY_TYPE_STATUS,
			"activity_by": map[string]any{"uid": base["ada"]}, "activity_time": rfc(at), "activity_next_state": "todo"})
	}
	dg.Mutate(t, map[string]any{"uid": uidOf[hid], "task_activities": lines})
	hp := map[string]string{"task_uuid": hid}
	var panel dgraphStruct.DgraphTask
	if code := pastCall(t, taskController.GetTaskInfo, "GET", "/task/info/"+hid, nil, ada1, hp, &panel); code != http.StatusOK {
		t.Fatalf("task info: %d", code)
	}
	if len(panel.Activity) != 1000 || panel.ActivityCount != 1100 || !panel.ActivityHasMore {
		t.Fatalf("panel: %d lines of %d, more %v; want the newest 1000 of 1100", len(panel.Activity), panel.ActivityCount, panel.ActivityHasMore)
	}
	if panel.Activity[len(panel.Activity)-1].Uuid != lineIDs[1099] {
		t.Fatalf("the panel's last line is not the newest")
	}
	history := panel.Activity
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("history never ends")
		}
		var older dgraphStruct.DgraphTask
		target := fmt.Sprintf("/task/getTaskActivityList/%s?before=%s&first=50", hid, history[0].Uuid)
		if code := pastCall(t, taskController.GetTaskActivityList, "GET", target, nil, ada1, hp, &older); code != http.StatusOK {
			t.Fatalf("%s: %d", target, code)
		}
		history = append(older.Activity, history...)
		if !older.ActivityHasMore {
			break
		}
	}
	if len(history) != 1100 {
		t.Fatalf("history paged back to %d lines, want 1100", len(history))
	}
	seenLine := map[string]bool{}
	for i, l := range history {
		if seenLine[l.Uuid] {
			t.Fatalf("line %s read twice", l.Uuid)
		}
		seenLine[l.Uuid] = true
		if i > 0 && l.LogTime.Before(*history[i-1].LogTime) {
			t.Fatalf("line %d is older than the one before it", i)
		}
	}
	if code := pastCall(t, taskController.GetTaskActivityList, "GET", fmt.Sprintf("/task/getTaskActivityList/%s?before=%s", hid, uuid.NewString()), nil, ada1, hp, nil); code != http.StatusBadRequest {
		t.Fatalf("a line the task doesn't have: %d, want 400", code)
	}
	// Someone outside the project learns nothing, not even which lines exist.
	bob1 := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: bob, EmailID: "bob@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base["bob"], Uuid: bob.String(), UserName: "bob"},
	}
	for _, line := range []string{lineIDs[500], uuid.NewString()} {
		if code := pastCall(t, taskController.GetTaskActivityList, "GET", fmt.Sprintf("/task/getTaskActivityList/%s?before=%s", hid, line), nil, bob1, hp, nil); code != http.StatusForbidden {
			t.Fatalf("history before %s for someone outside the project: %d, want 403", line, code)
		}
	}

	// Reordering in a column of 2,250 with no room between the neighbours
	// renumbers every card in it, so its order stays whole.
	before, after, moved := allOwn[11], allOwn[10], allOwn[500]
	if code := pastCall(t, taskController.MoveTask, "POST", "/task/moveTask", map[string]string{"task_uuid": moved.Uuid, "task_status": "todo",
		"before_task_uuid": before.Uuid, "after_task_uuid": after.Uuid}, ada1, nil, nil); code != http.StatusOK {
		t.Fatalf("move: %d", code)
	}
	raw := dg.Query(t, fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { t: project_tasks @filter(eq(task_status, "todo")) { task_uuid task_rank } } }`, P))
	var ranked struct {
		P []struct {
			T []struct {
				ID   string   `json:"task_uuid"`
				Rank *float64 `json:"task_rank"`
			} `json:"t"`
		} `json:"p"`
	}
	if err := json.Unmarshal(raw, &ranked); err != nil || len(ranked.P) == 0 {
		t.Fatalf("ranks: %v %s", err, raw)
	}
	rankOf := map[string]float64{}
	distinct := map[float64]bool{}
	for _, r := range ranked.P[0].T {
		if r.Rank == nil {
			t.Fatalf("task %s was left out of the renumbering", r.ID)
		}
		rankOf[r.ID] = *r.Rank
		distinct[*r.Rank] = true
	}
	if len(rankOf) != 2250 || len(distinct) != 2250 {
		t.Fatalf("%d tasks ranked, %d different ranks; want 2250 of each", len(rankOf), len(distinct))
	}
	var after2 dgraphStruct.DgraphProject
	pastCall(t, projectController.GetProjectTaskListForKanban, "GET", "/project/taskListForKanban/"+P.String()+"?openLimit=5000", nil, ada1, pp, &after2)
	reOwn, _ := split(after2.TasksTodo)
	for i, c := range reOwn {
		if c.Uuid == before.Uuid {
			if i+1 >= len(reOwn) || reOwn[i+1].Uuid != moved.Uuid {
				t.Fatalf("the moved card is not right below the one it was dropped under")
			}
		}
	}
	ordered := append([]*dgraphStruct.DgraphTask{}, reOwn...)
	sort.SliceStable(ordered, func(i, j int) bool { return rankOf[ordered[i].Uuid] < rankOf[ordered[j].Uuid] })
	for i := range ordered {
		if ordered[i].Uuid != reOwn[i].Uuid {
			t.Fatalf("the board's order and the ranks disagree at card %d", i)
		}
	}
}
