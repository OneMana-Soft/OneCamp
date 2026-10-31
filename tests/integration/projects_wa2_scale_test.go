//go:build integration
// +build integration

package integration_test

// Restricted tasks at scale (WA2, design/projects-vs-asana-jira/WA2_PLAN.md):
// the filter is one batched read per request, never one per task, so a board
// that reads hundreds of tasks at once (progressive columns, a closed cap of
// 200) asks Postgres as many times as a board of a few; and where no task is
// restricted it costs close to nothing.
//
// TestWA2AReadsAskPostgresAFixedNumberOfTimes is in the suite. The timings
// (TestWA2AReadTimesAtScale) run when WA2_MEASURE=1 is set.
//
// Run: go test -tags=integration ./tests/integration/ -run TestWA2AReads -v
//      WA2_MEASURE=1 go test -tags=integration ./tests/integration/ -run TestWA2AReadTimesAtScale -v

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// wa2Scale is one big project: Ana runs it, Ben is a member, and its tasks
// are spread over the board's columns; some are dated, some are Ben's.
type wa2Scale struct {
	t       *testing.T
	env     *integration.Env
	dg      *integration.DgraphEnv
	project string
	uids    map[string]string // ana, ben, project: the graph's
	people  map[string]uuid.UUID
	tasks   []string
	// counter, when a test counts Postgres statements, marks each request's
	// context, so a request's own statements are what it counts (request).
	counter *integration.PostgresCounter
}

// request is a request's context: counted, when the test counts.
func (s *wa2Scale) request(c context.Context) context.Context {
	if s.counter == nil {
		return c
	}
	return s.counter.Context(c)
}

func newWA2Scale(t *testing.T, env *integration.Env, dg *integration.DgraphEnv) *wa2Scale {
	t.Helper()
	s := &wa2Scale{t: t, env: env, dg: dg, project: uuid.NewString(), people: map[string]uuid.UUID{}}
	live := "0001-01-01T00:00:00Z"
	team := uuid.New()
	for _, name := range []string{"ana", "ben"} {
		s.account(name)
	}
	s.exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Scale', $2)`, team, s.people["ana"])
	s.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Big board', $2, $3)`, s.project, team, s.people["ana"])
	user := func(name string) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": s.people[name].String(), "user_name": name,
			"user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:p"}}, "user_teams": []map[string]any{{"uid": "_:team"}}}
	}
	s.uids = dg.Mutate(t, []map[string]any{
		user("ana"), user("ben"),
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Scale", "team_deleted_at": live,
			"team_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}}, "team_admins": []map[string]any{{"uid": "_:ana"}},
			"team_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": s.project, "project_name": "Big board", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}}, "project_admins": []map[string]any{{"uid": "_:ana"}},
			"project_created_by": map[string]any{"uid": "_:ana"}, "project_team": map[string]any{"uid": "_:team"}},
	})
	return s
}

func (s *wa2Scale) exec(q string, args ...any) {
	s.t.Helper()
	if _, err := s.env.PG.Exec(q, args...); err != nil {
		s.t.Fatalf("%s: %v", q, err)
	}
}

// account adds name's row to the workspace's users, as s.people[name].
func (s *wa2Scale) account(name string) {
	s.t.Helper()
	s.people[name] = uuid.New()
	s.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@scale.test', $2, $2, NOW(), NOW())`, s.people[name], name)
}

// person adds someone to the workspace, in Postgres and the graph, and
// answers their graph uid. A viewer is also a viewer of s's project;
// anyone else is outside it.
func (s *wa2Scale) person(name string, viewer bool) string {
	s.t.Helper()
	s.account(name)
	user := map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": s.people[name].String(), "user_name": name,
		"user_deleted_at": "0001-01-01T00:00:00Z"}
	nodes := []map[string]any{user}
	if viewer {
		user["user_projects"] = []map[string]any{{"uid": s.uids["p"]}}
		nodes = append(nodes, map[string]any{"uid": s.uids["p"], "project_members": []map[string]any{{"uid": "_:" + name}}})
	}
	uid := s.dg.Mutate(s.t, nodes)[name]
	if viewer {
		s.exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, s.project, s.people[name])
	}
	return uid
}

// addTasks adds n tasks to the project, as the app makes them: a row in
// Postgres and a node in the graph. Statuses go round the board's columns;
// every second one is dated (for the timeline), every third one is Ben's.
func (s *wa2Scale) addTasks(n int) {
	s.t.Helper()
	live := "0001-01-01T00:00:00Z"
	statuses := []string{"todo", "inProgress", "inReview", "backlog", "done", "canceled"}
	now := time.Now().UTC()
	var nodes, onProject, onBen []map[string]any
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.NewString()
		k := len(s.tasks) + i
		key := fmt.Sprintf("_:t%d", k)
		node := map[string]any{"uid": key, "dgraph.type": "Task", "task_uuid": id, "task_name": fmt.Sprintf("Task %d", k),
			"task_status": statuses[k%len(statuses)], "task_priority": "medium", "task_deleted_at": live,
			"task_created_at": now.Add(-time.Duration(k) * time.Minute).Format(time.RFC3339),
			"task_project":    map[string]any{"uid": s.uids["p"]}, "task_created_by": map[string]any{"uid": s.uids["ana"]}}
		if k%2 == 0 {
			node["task_start_date"] = now.AddDate(0, 0, k%20-10).Format(time.RFC3339)
			node["task_due_date"] = now.AddDate(0, 0, k%20-5).Format(time.RFC3339)
		}
		if k%3 == 0 {
			node["task_assignee"] = map[string]any{"uid": s.uids["ben"]}
			onBen = append(onBen, map[string]any{"uid": key})
		}
		nodes = append(nodes, node)
		onProject = append(onProject, map[string]any{"uid": key})
		ids = append(ids, id)
	}
	nodes = append(nodes, map[string]any{"uid": s.uids["p"], "project_tasks": onProject})
	if len(onBen) > 0 {
		nodes = append(nodes, map[string]any{"uid": s.uids["ben"], "user_tasks": onBen})
	}
	s.dg.Mutate(s.t, nodes)
	s.exec(`INSERT INTO tasks (id, project_id, created_by) SELECT unnest($1::uuid[]), $2, $3`, "{"+strings.Join(ids, ",")+"}", s.project, s.people["ana"])
	s.tasks = append(s.tasks, ids...)
}

// restrict keeps n of the open, dated tasks that aren't Ben's to the
// project's admins (tasks kept from Ben), or with n = 0 opens them all again.
// Rows, as the API leaves them. False where the server has no restricted
// tasks (a build before WA2).
func (s *wa2Scale) restrict(n int) bool {
	s.t.Helper()
	var exists bool
	if err := s.env.PG.QueryRow(`SELECT to_regclass('task_restrictions') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return false
	}
	s.exec(`DELETE FROM task_restrictions WHERE project_id = $1`, s.project)
	// Every sixth task from the third: in review, dated, nobody's.
	for i := 0; i < n && i*6+2 < len(s.tasks); i++ {
		s.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, s.tasks[i*6+2], s.project, s.people["ana"])
	}
	return true
}

// call runs a handler as who, through the request middleware (which gives
// the request its one filter), with the route parameters given, raw as its
// body (nil for none) and target as its URL: the one way every test here
// calls a handler.
func (s *wa2Scale) call(who, method, target string, h http.HandlerFunc, params map[string]string, raw []byte) (int, string) {
	s.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people[who])
	if err != nil || pg == nil {
		s.t.Fatalf("load %s: %v", who, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people[who].String())
	if err != nil || gr == nil {
		s.t.Fatalf("load %s's profile: %v", who, err)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
	var body io.Reader
	if raw != nil {
		body = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(method, target, body).WithContext(s.request(c)))
	return rec.Code, rec.Body.String()
}

// read runs a handler as who, through the request middleware (which gives the
// request its one filter), with the project's id as a route parameter.
func (s *wa2Scale) read(who string, h http.HandlerFunc, query string) (int, string) {
	s.t.Helper()
	return s.call(who, http.MethodGet, "/"+query, h, map[string]string{"project_uuid": s.project}, nil)
}

// wa2Reads are the reads of many tasks at once: the board (progressive
// columns, a closed cap of 200), My Tasks' first page, the timeline and the
// project's list.
func wa2Reads(s *wa2Scale) map[string]func(who string) (int, string) {
	return map[string]func(who string) (int, string){
		"board": func(who string) (int, string) { return s.read(who, projectController.GetProjectTaskListForKanban, "") },
		"my tasks": func(who string) (int, string) {
			return s.read(who, userController.GetDgraphUserTaskList, "?pageSize=50&pageIndex=0")
		},
		"timeline": func(who string) (int, string) { return s.read(who, projectController.ProjectTimeline, "") },
		"list": func(who string) (int, string) {
			return s.read(who, projectController.GetProjectTaskList, "?pageSize=50&pageIndex=0")
		},
	}
}

func TestWA2AReadsAskPostgresAFixedNumberOfTimes(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.counter = counter
	reads := wa2Reads(s)
	// Postgres statements each read sends, as each of them: the most any of
	// them may ever send, whatever is restricted and however many tasks.
	const most = 12
	countAll := func() map[string]int {
		t.Helper()
		got := map[string]int{}
		for name, read := range reads {
			for _, who := range []string{"ben", "ana"} {
				read(who) // a first read warms what is kept a while (policies, keys)
				counter.Reset()
				code, out := read(who)
				if code != http.StatusOK {
					t.Fatalf("%s as %s: %d %s", name, who, code, out)
				}
				got[name+" as "+who] = counter.Count()
			}
		}
		return got
	}

	s.addTasks(60)
	if !s.restrict(3) {
		t.Fatal("no task_restrictions table")
	}
	few := countAll()
	s.addTasks(540)
	if !s.restrict(3) {
		t.Fatal("no task_restrictions table")
	}
	many := countAll()
	s.restrict(0)
	none := countAll()
	keys := make([]string, 0, len(few))
	for k := range few {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-20s Postgres statements: %d with 60 tasks, %d with 600, %d with none restricted", k, few[k], many[k], none[k])
		if many[k] != few[k] {
			t.Errorf("%s: %d statements for 600 tasks, %d for 60: a read asks per task", k, many[k], few[k])
		}
		if many[k] > most || none[k] > most {
			t.Errorf("%s: %d statements (%d with none restricted), more than %d", k, many[k], none[k], most)
		}
	}
}

// wa2FilterOff, where this build has the filter (projects_wa2_filter_test.go),
// turns its decision off until the func it returns is called, or the test
// ends: what a read costs with nothing deciding and nothing left out. Nil in a
// build before WA2.
var wa2FilterOff func(t *testing.T) (restore func())

func TestWA2AReadTimesAtScale(t *testing.T) {
	if os.Getenv("WA2_MEASURE") == "" {
		t.Skip("set WA2_MEASURE=1 to measure")
	}
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	for i := 0; i < 6; i++ {
		s.addTasks(500)
	}
	reads := wa2Reads(s)
	const runs = 40
	type scenario struct {
		name  string
		setup func() (undo func())
	}
	scenarios := []scenario{{"no task restricted", func() func() { s.restrict(0); return func() {} }}}
	if s.restrict(0) {
		scenarios = append(scenarios, scenario{"5 restricted, kept from the reader", func() func() { s.restrict(5); return func() {} }})
	}
	if wa2FilterOff != nil {
		scenarios = append(scenarios, scenario{"5 restricted, the filter off", func() func() { s.restrict(5); return wa2FilterOff(t) }})
	}
	names := []string{"board", "my tasks", "timeline"}
	took := map[string]map[string][]time.Duration{}
	for _, name := range names {
		took[name] = map[string][]time.Duration{}
		for i := 0; i < runs+3; i++ {
			// The scenarios take turns, so each meets the same load.
			for _, sc := range scenarios {
				undo := sc.setup()
				start := time.Now()
				code, out := reads[name]("ben")
				d := time.Since(start)
				undo()
				if code != http.StatusOK {
					t.Fatalf("%s (%s): %d %.300s", name, sc.name, code, out)
				}
				if i >= 3 { // three to warm up
					took[name][sc.name] = append(took[name][sc.name], d)
				}
			}
		}
	}
	pct := func(ds []time.Duration, p float64) time.Duration {
		sorted := append([]time.Duration(nil), ds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[int(float64(len(sorted)-1)*p+0.5)]
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	t.Logf("3,000 tasks in one project (1,000 Ben's, 1,500 dated); %d runs each, as Ben, a member", runs)
	for _, name := range names {
		first := took[name][scenarios[0].name]
		for _, sc := range scenarios {
			ds := took[name][sc.name]
			// The runs took turns, so each run's difference from the first
			// scenario's met the same load: its median is the steadier sign.
			diffs := make([]time.Duration, len(ds))
			for i := range ds {
				diffs[i] = ds[i] - first[i]
			}
			t.Logf("%-9s %-36s p50 %6.1f ms  p95 %6.1f ms  paired median vs %q %+6.1f ms", name, sc.name,
				ms(pct(ds, 0.5)), ms(pct(ds, 0.95)), scenarios[0].name, ms(pct(diffs, 0.5)))
		}
	}
}
