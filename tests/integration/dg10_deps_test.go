//go:build integration

package integration_test

// DG10: dependencies across projects, the rights rule, and a move that
// pushes its chain along all at once or not at all.
// Run: integ_rel.sh <wt> <log> 'TestDG10'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// dg10World is two projects of one team. Launch (P): Ana runs it, Ben and
// Cat are in it, Cat only to read. Marketing (Q): Ana runs it, Cat works in
// it, Ben isn't in it.
type dg10World struct {
	t      *testing.T
	env    *integration.Env
	dg     *integration.DgraphEnv
	p, q   string
	people map[string]uuid.UUID
	uids   map[string]string
	tasks  map[string]string // name → task uuid
}

func newDG10World(t *testing.T) *dg10World {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	w := &dg10World{t: t, env: env, dg: dg, p: uuid.NewString(), q: uuid.NewString(), people: map[string]uuid.UUID{}, tasks: map[string]string{}}
	live := "0001-01-01T00:00:00Z"
	team := uuid.New()
	for _, name := range []string{"ana", "ben", "cat"} {
		w.people[name] = uuid.New()
		w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@dg10.test', $2, $2, NOW(), NOW())`, w.people[name], name)
	}
	w.exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Work', $2)`, team, w.people["ana"])
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Launch', $2, $3), ($4, 'Marketing', $2, $3)`, w.p, team, w.people["ana"], w.q)
	w.exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, w.p, w.people["cat"])
	user := func(name string, projects ...string) map[string]any {
		var ps []map[string]any
		for _, p := range projects {
			ps = append(ps, map[string]any{"uid": p})
		}
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": w.people[name].String(), "user_name": name,
			"user_deleted_at": live, "user_projects": ps, "user_teams": []map[string]any{{"uid": "_:team"}}}
	}
	ref := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	w.uids = dg.Mutate(t, []map[string]any{
		user("ana", "_:p", "_:q"), user("ben", "_:p"), user("cat", "_:p", "_:q"),
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Work", "team_deleted_at": live,
			"team_members": ref("ana", "ben", "cat"), "team_admins": ref("ana"), "team_projects": ref("p", "q")},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": w.p, "project_name": "Launch", "project_deleted_at": live,
			"project_members": ref("ana", "ben", "cat"), "project_admins": ref("ana"),
			"project_created_by": map[string]any{"uid": "_:ana"}, "project_team": map[string]any{"uid": "_:team"}},
		{"uid": "_:q", "dgraph.type": "Project", "project_uuid": w.q, "project_name": "Marketing", "project_deleted_at": live,
			"project_members": ref("ana", "cat"), "project_admins": ref("ana"),
			"project_created_by": map[string]any{"uid": "_:ana"}, "project_team": map[string]any{"uid": "_:team"}},
	})
	return w
}

func (w *dg10World) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.env.PG.Exec(q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// day is 09:00 or 17:00 UTC on a day of November 2026.
func dg10Day(d, hour int) time.Time { return time.Date(2026, 11, d, hour, 0, 0, 0, time.UTC) }

// task adds a task to project ("p" or "q") from day start to day due,
// assigned to assignee ("" for nobody), waiting finish to start on blockers
// (written straight to the graph, as data from before a rule would be).
func (w *dg10World) task(name, project string, start, due int, assignee string, blockers ...string) {
	w.t.Helper()
	w.taskAt(name, project, dg10Day(start, 9), dg10Day(due, 17), nil, assignee, blockers...)
}

// taskAt is task with its days as times (zero: no date), and more of its
// predicates in more (its status, say).
func (w *dg10World) taskAt(name, project string, start, due time.Time, more map[string]any, assignee string, blockers ...string) {
	w.t.Helper()
	id := uuid.NewString()
	pid := w.p
	if project == "q" {
		pid = w.q
	}
	node := map[string]any{"uid": "_:" + name, "dgraph.type": "Task", "task_uuid": id, "task_name": name, "task_status": "todo",
		"task_deleted_at": "0001-01-01T00:00:00Z", "task_created_at": time.Now().UTC().Format(time.RFC3339),
		"task_project": map[string]any{"uid": w.uids[project]}, "task_created_by": map[string]any{"uid": w.uids["ana"]}}
	if !start.IsZero() {
		node["task_start_date"] = start.Format(time.RFC3339)
	}
	if !due.IsZero() {
		node["task_due_date"] = due.Format(time.RFC3339)
	}
	for k, v := range more {
		node[k] = v
	}
	if assignee != "" {
		node["task_assignee"] = map[string]any{"uid": w.uids[assignee]}
	}
	var by []map[string]any
	for _, b := range blockers {
		by = append(by, map[string]any{"uid": w.uids[b]})
	}
	if len(by) > 0 {
		node["task_blocked_by"] = by
	}
	nodes := []map[string]any{node, {"uid": w.uids[project], "project_tasks": []map[string]any{{"uid": "_:" + name}}}}
	if assignee != "" {
		// A person's own tasks are read through user_tasks, as the app writes it.
		nodes = append(nodes, map[string]any{"uid": w.uids[assignee], "user_tasks": []map[string]any{{"uid": "_:" + name}}})
	}
	got := w.dg.Mutate(w.t, nodes)
	w.uids[name] = got[name]
	w.tasks[name] = id
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, pid, w.people["ana"])
}

// call runs a handler as who (through a token with token), with route
// params and a JSON body (nil for none), through the request middleware.
func (w *dg10World) call(who string, token bool, h http.HandlerFunc, method, target string, params map[string]string, body any) (int, string) {
	w.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[who])
	if err != nil || pg == nil {
		w.t.Fatalf("load %s: %v", who, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, w.people[who].String())
	if err != nil || gr == nil {
		w.t.Fatalf("load %s's profile: %v", who, err)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
	if token {
		c = helpers.WithActsThroughToken(c)
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(method, target, bytes.NewReader(raw)).WithContext(c))
	return rec.Code, rec.Body.String()
}

// move drags name to start..due (days of November), its chain along.
func (w *dg10World) move(who string, token bool, name string, start, due int) (int, string) {
	w.t.Helper()
	return w.call(who, token, taskController.UpdateTaskDates, http.MethodPost, "/", nil, map[string]any{
		"task_uuid": w.tasks[name], "task_start_date": dg10Day(start, 9).Format(time.RFC3339),
		"task_due_date": dg10Day(due, 17).Format(time.RFC3339), "shift_dependents": true, "tz": "UTC"})
}

func (w *dg10World) link(who, name, on string) (int, string) {
	w.t.Helper()
	return w.call(who, false, taskController.AddTaskDependency, http.MethodPost, "/", nil, map[string]any{"task_uuid": w.tasks[name], "blocked_by_uuid": w.tasks[on]})
}

// days reads a task's start and due day of the month, and its history lines.
func (w *dg10World) days(name string) (start, due, lines int) {
	w.t.Helper()
	var out struct {
		Q []struct {
			Start time.Time `json:"task_start_date"`
			Due   time.Time `json:"task_due_date"`
			N     int       `json:"n"`
		} `json:"q"`
	}
	raw := w.dg.Query(w.t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_start_date task_due_date n: count(task_activities) } }`, w.tasks[name]))
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Q) != 1 {
		w.t.Fatalf("read %s: %v %s", name, err, raw)
	}
	return out.Q[0].Start.Day(), out.Q[0].Due.Day(), out.Q[0].N
}

// dg10Recorder is an MQTT client that keeps what is published.
type dg10Recorder struct {
	MQTT.Client
	mu   sync.Mutex
	sent []dg10Sent
}

type dg10Sent struct {
	topic string
	msg   struct {
		Type int8            `json:"type"`
		Data json.RawMessage `json:"data"`
	}
}

func (r *dg10Recorder) Publish(topic string, qos byte, retained bool, payload any) MQTT.Token {
	s := dg10Sent{topic: topic}
	if b, ok := payload.([]byte); ok {
		_ = json.Unmarshal(b, &s.msg)
	}
	r.mu.Lock()
	r.sent = append(r.sent, s)
	r.mu.Unlock()
	return r.Client.Publish(topic, qos, retained, payload)
}

// dates is the task-dates messages sent, after a moment for the goroutines
// that send them.
func (r *dg10Recorder) dates() []dg10Sent {
	time.Sleep(300 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []dg10Sent
	for _, s := range r.sent {
		if s.msg.Type == mqttStruct.MESSAGE_TASK_DATES {
			out = append(out, s)
		}
	}
	return out
}

func (r *dg10Recorder) reset() {
	time.Sleep(300 * time.Millisecond)
	r.mu.Lock()
	r.sent = nil
	r.mu.Unlock()
}

func dg10Record(t *testing.T) *dg10Recorder {
	t.Helper()
	r := &dg10Recorder{Client: MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1"))}
	integration.UseMqttClient(t, r)
	return r
}

// A drag that pushes its chain along writes all of it or none of it: here
// Postgres refuses one task of the chain, and neither the dragged task nor
// the chain moves. Then, refused nothing, the whole move lands at once, with
// one message per project told.
func TestDG10AMoveIsAllOrNothing(t *testing.T) {
	w := newDG10World(t)
	rec := dg10Record(t)
	w.task("a", "p", 2, 4, "ana")
	w.task("b", "p", 5, 7, "", "a")
	w.task("c", "p", 8, 9, "", "b")
	w.task("x", "q", 5, 6, "", "a") // Marketing waits on Launch's a
	w.exec(`CREATE OR REPLACE FUNCTION dg10_refuse() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'dg10: refused'; END $$ LANGUAGE plpgsql`)
	w.exec(fmt.Sprintf(`CREATE TRIGGER dg10_refuse BEFORE UPDATE ON tasks FOR EACH ROW WHEN (OLD.id = '%s') EXECUTE FUNCTION dg10_refuse()`, w.tasks["c"]))

	code, body := w.move("ana", false, "a", 3, 6)
	if code == http.StatusOK {
		t.Errorf("a move whose chain couldn't be written says it worked: %d %s", code, body)
	}
	for name, want := range map[string][2]int{"a": {2, 4}, "b": {5, 7}, "c": {8, 9}, "x": {5, 6}} {
		if s, d, _ := w.days(name); s != want[0] || d != want[1] {
			t.Errorf("%s moved though the move failed: %d-%d, want %d-%d", name, s, d, want[0], want[1])
		}
	}

	w.exec(`DROP TRIGGER dg10_refuse ON tasks`)
	rec.reset()
	code, body = w.move("ana", false, "a", 3, 6)
	if code != http.StatusOK {
		t.Fatalf("the move: %d %s", code, body)
	}
	for name, want := range map[string][2]int{"a": {3, 6}, "b": {7, 9}, "c": {10, 11}, "x": {7, 8}} {
		if s, d, lines := w.days(name); s != want[0] || d != want[1] || lines != 2 {
			t.Errorf("%s: %d-%d with %d lines, want %d-%d with 2 (a start and a due line)", name, s, d, lines, want[0], want[1])
		}
	}
	if !strings.Contains(body, w.tasks["x"]) {
		t.Errorf("Ana runs Marketing too, so its x moves along and says so: %s", body)
	}
	byTopic := map[string]int{}
	for _, s := range rec.dates() {
		byTopic[s.topic]++
		var d struct {
			Project string `json:"project_uuid"`
			Tasks   []struct {
				UUID string `json:"task_uuid"`
			} `json:"tasks"`
		}
		_ = json.Unmarshal(s.msg.Data, &d)
		want := 3 // a, b, c (a topic names its project encoded, so read the message's)
		if d.Project == w.q {
			want = 1
		}
		if len(d.Tasks) != want {
			t.Errorf("the message on %s lists %d tasks, want %d: %s", s.topic, len(d.Tasks), want, s.msg.Data)
		}
	}
	if len(byTopic) != 2 {
		t.Errorf("two projects moved, two topics told: %v", byTopic)
	}
	for topic, n := range byTopic {
		if n != 1 {
			t.Errorf("%s was told %d times for one move, want once", topic, n)
		}
	}
}

// The cross-project rule: a link across projects where the person can change
// tasks in both; a task in a project they can't see is not there for them,
// in a link, in a task's panel, on the timeline and in a move.
func TestDG10DependenciesAcrossProjects(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("a", "p", 2, 4, "ana")
	w.task("b", "p", 5, 7, "", "a")
	w.task("c", "p", 20, 21, "")
	w.task("x", "q", 5, 6, "", "a") // written before the rule: Marketing waits on a
	w.task("y", "q", 10, 12, "")

	for _, c := range []struct {
		who, name, on string
		code          int
		words         string
	}{
		{"ana", "y", "b", http.StatusOK, ""},                // she runs both
		{"ana", "c", "y", http.StatusOK, ""},                // and either way round
		{"ben", "b", "y", http.StatusNotFound, "not found"}, // Marketing isn't his to see
		{"cat", "y", "b", http.StatusForbidden, "You can link tasks in two projects only when you can change tasks in both."}, // she only reads Launch
		{"cat", "b", "y", http.StatusForbidden, ""}, // and can't change Launch's b
	} {
		code, body := w.link(c.who, c.name, c.on)
		if code != c.code || (c.words != "" && !strings.Contains(body, c.words)) {
			t.Errorf("%s: %s waits on %s: %d %s, want %d %q", c.who, c.name, c.on, code, body, c.code, c.words)
		}
	}

	// a's panel: Ana sees x (Marketing) waiting on it; Ben doesn't, not
	// even that something hidden does.
	panel := func(who string) string {
		code, body := w.call(who, false, taskController.GetTaskInfo, http.MethodGet, "/", map[string]string{"task_uuid": w.tasks["a"]}, nil)
		if code != http.StatusOK {
			t.Fatalf("%s reads a: %d %s", who, code, body)
		}
		return body
	}
	if body := panel("ana"); !strings.Contains(body, w.tasks["x"]) {
		t.Errorf("Ana sees Marketing's x waiting on a: %s", body)
	}
	if body := panel("ben"); strings.Contains(body, w.tasks["x"]) || !strings.Contains(body, w.tasks["b"]) {
		t.Errorf("Ben sees Launch's b waiting on a, and nothing of Marketing's x: %s", body)
	}
	// The timeline: c waits on Marketing's y; Ben's timeline doesn't name it.
	code, body := w.call("ben", false, projectController.ProjectTimeline, http.MethodGet, "/", map[string]string{"project_uuid": w.p}, nil)
	if code != http.StatusOK || strings.Contains(body, w.tasks["y"]) || strings.Contains(body, w.tasks["x"]) {
		t.Errorf("Ben's timeline names no Marketing task: %d %s", code, body)
	}

	// Ben moves a: b moves; x, in Marketing, isn't his to move.
	if code, body := w.move("ben", false, "a", 3, 6); code != http.StatusOK || strings.Contains(body, w.tasks["x"]) {
		t.Fatalf("Ben's move: %d %s", code, body)
	}
	if s, _, _ := w.days("b"); s != 7 {
		t.Errorf("b moves along for Ben: starts on %d", s)
	}
	if s, _, _ := w.days("x"); s != 5 {
		t.Errorf("x stays: Ben can't change Marketing (starts on %d)", s)
	}
	// Ana moves it again: x is hers to move, and y after b.
	if code, body := w.move("ana", false, "a", 4, 9); code != http.StatusOK || !strings.Contains(body, w.tasks["x"]) {
		t.Fatalf("Ana's move: %d %s", code, body)
	}
	if s, _, _ := w.days("x"); s != 10 {
		t.Errorf("x moves along for Ana: starts on %d", s)
	}
	if s, _, _ := w.days("y"); s != 13 {
		t.Errorf("y, waiting on b across projects, moves along for Ana: starts on %d", s)
	}
}

// Through an API token, a person acts as a member at most (memory "tokens
// act as members"): in a project where members change only the tasks
// assigned to them, Ana's token moves her own task and none of the chain
// that isn't hers; in the app, as its admin, she moves the chain.
func TestDG10TokensActAsMembers(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.exec(`INSERT INTO policy_scoped_values (scope_kind, scope_id, key, value) VALUES ('project', $1, 'projects.edit.who', 'assigned')`, w.p)
	w.task("a", "p", 2, 4, "ana")
	w.task("b", "p", 5, 7, "ben", "a")
	code, body := w.move("ana", true, "a", 3, 6)
	if code != http.StatusOK {
		t.Fatalf("her token moves her own task: %d %s", code, body)
	}
	if s, _, _ := w.days("b"); s != 5 || strings.Contains(body, w.tasks["b"]) {
		t.Errorf("her token can't move Ben's b: starts on %d; %s", s, body)
	}
	code, body = w.move("ana", false, "a", 4, 7)
	if code != http.StatusOK || !strings.Contains(body, w.tasks["b"]) {
		t.Fatalf("in the app she runs the project: %d %s", code, body)
	}
	if s, _, _ := w.days("b"); s != 8 {
		t.Errorf("b moves along in the app: starts on %d", s)
	}
}
