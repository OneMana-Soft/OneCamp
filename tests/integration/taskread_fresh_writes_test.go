//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/google/uuid"
)

// H2WF (MAIN, 17 Nov: writes read fresh). H2 keeps the rows that decide who
// may see a task between requests (models/taskread/generation). A write must
// never be let in by such an entry: here every change to those rows is made
// as another process whose bump was lost (Redis failing the write, H2's
// fail-open), so a kept entry still says what was true before it, for as long
// as the clock is held (inside MaxAge).

// freshWorld is a task (ana's) under a parent, in the decision world's
// project, and ben, a member, who acts on it through the real middleware and
// controllers.
type freshWorld struct {
	*decisionWorld
	ben, benUID string
	gen         func()
}

func newFreshWorld(t *testing.T) *freshWorld {
	d := newDecisionWorld(t)
	f := &freshWorld{decisionWorld: d, ben: d.people["ben"].String()}
	f.benUID = f.uid(t, "user_uuid", f.ben)
	f.gen = func() {
		t.Helper()
		if err := redisInit.RedisClient.Incr(context.Background(), "taskread:gen").Err(); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *freshWorld) uid(t *testing.T, pred, id string) string {
	t.Helper()
	var out struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(f.dg.Query(t, fmt.Sprintf(`{ q(func: eq(%s, %q)) { uid } }`, pred, id)), &out); err != nil || len(out.Q) != 1 {
		t.Fatalf("the node with %s %s: %+v %v", pred, id, out, err)
	}
	return out.Q[0].UID
}

// chain is a task under each task above it (the first is the top), all
// ana's, todo, in the project; it answers their uuids, top first.
func (f *freshWorld) chain(t *testing.T, depth int) []string {
	t.Helper()
	live := "0001-01-01T00:00:00Z"
	project, ana := f.uid(t, "project_uuid", f.project), f.uid(t, "user_uuid", f.people["ana"].String())
	ids := make([]string, depth)
	var nodes []map[string]any
	var tasks []map[string]any
	for i := range ids {
		ids[i] = uuid.NewString()
		f.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, ids[i], f.project, f.people["ana"])
		n := map[string]any{"uid": fmt.Sprintf("_:t%d", i), "dgraph.type": "Task", "task_uuid": ids[i], "task_name": fmt.Sprintf("Step %d", i),
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": project},
			"task_created_by": map[string]any{"uid": ana}}
		if i > 0 {
			n["task_parent_task"] = map[string]any{"uid": fmt.Sprintf("_:t%d", i-1)}
		}
		nodes = append(nodes, n)
		tasks = append(tasks, map[string]any{"uid": fmt.Sprintf("_:t%d", i)})
	}
	nodes = append(nodes, map[string]any{"uid": project, "project_tasks": tasks})
	f.dg.Mutate(t, nodes)
	return ids
}

// restrict restricts task (with via, the task it is restricted with, or
// "") and lets in grants, by name, as rows only: no bump.
func (f *freshWorld) restrict(task, via string, grants ...string) {
	var v any
	if via != "" {
		v = via
	}
	f.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by, via_task_id) VALUES ($1, $2, $3, $4)`, task, f.project, f.people["ana"], v)
	for _, g := range grants {
		f.exec(`INSERT INTO task_restriction_grants (task_id, user_id) VALUES ($1, $2)`, task, f.people[g])
	}
}

// write is one request as ben through the middleware a route has
// (RequestIDMiddleware, VerifyAuth) to h, with body; it answers the status.
func (f *freshWorld) write(t *testing.T, h http.HandlerFunc, target, body string) int {
	t.Helper()
	session, err := helpers.SignSessionToken(f.ben, helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "Authorization", Value: session})
	req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
	rec := httptest.NewRecorder()
	middleware.RequestIDMiddleware(middleware.VerifyAuth(h)).ServeHTTP(rec, req)
	return rec.Code
}

type freshRoute struct {
	name string
	call func(f *freshWorld, t *testing.T, task string) int
}

var freshRoutes = []freshRoute{
	{"status change", func(f *freshWorld, t *testing.T, task string) int {
		return f.write(t, taskController.UpdateTaskStatus, "/task/updateTaskStatus",
			fmt.Sprintf(`{"task_uuid":%q,"task_project_uuid":%q,"task_status":"inReview"}`, task, f.project))
	}},
	{"drag", func(f *freshWorld, t *testing.T, task string) int {
		return f.write(t, taskController.MoveTask, "/task/moveTask",
			fmt.Sprintf(`{"task_uuid":%q,"task_status":"todo","before_task_uuid":"","after_task_uuid":""}`, task))
	}},
}

// The changes to the rows, each made after ben's read kept what was true
// before it. allowed is whether ben may then write the task (the last of
// the chain).
type freshCase struct {
	name    string
	depth   int
	before  func(f *freshWorld, ids []string)
	change  func(f *freshWorld, ids []string)
	allowed bool
	cold    bool // no read before the change: nothing kept
}

var freshCases = []freshCase{
	{name: "grant on the parent revoked", depth: 2,
		before: func(f *freshWorld, ids []string) { f.restrict(ids[0], "", "ben"); f.restrict(ids[1], ids[0]) },
		change: func(f *freshWorld, ids []string) {
			f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
		}},
	{name: "grant two levels up revoked", depth: 3,
		before: func(f *freshWorld, ids []string) {
			f.restrict(ids[0], "", "ben")
			f.restrict(ids[1], ids[0])
			f.restrict(ids[2], ids[0])
		},
		change: func(f *freshWorld, ids []string) {
			f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
		}},
	{name: "restriction added to the parent", depth: 2,
		before: func(*freshWorld, []string) {},
		change: func(f *freshWorld, ids []string) { f.restrict(ids[0], ""); f.restrict(ids[1], ids[0]) }},
	{name: "restriction on the parent removed", depth: 2, allowed: true,
		before: func(f *freshWorld, ids []string) { f.restrict(ids[0], ""); f.restrict(ids[1], ids[0]) },
		change: func(f *freshWorld, ids []string) {
			f.exec(`DELETE FROM task_restrictions WHERE task_id = ANY($1)`, "{"+ids[1]+","+ids[0]+"}")
		}},
	{name: "grant on the task itself revoked", depth: 1,
		before: func(f *freshWorld, ids []string) { f.restrict(ids[0], "", "ben") },
		change: func(f *freshWorld, ids []string) {
			f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
		}},
	{name: "grant on the parent revoked, nothing kept", depth: 2, cold: true,
		before: func(f *freshWorld, ids []string) { f.restrict(ids[0], "", "ben"); f.restrict(ids[1], ids[0]) },
		change: func(f *freshWorld, ids []string) {
			f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
		}},
}

// TestAWriteDecidesWhoMaySeeATaskFromTheRows: ben's status change and drag
// answer, inside the window a kept entry is served in, what they answer
// once nothing is kept (the shared generation moved): refused as someone the
// task is hidden from, or let in at once. A read in the same window is still
// served from what was kept (H2 unchanged).
func TestAWriteDecidesWhoMaySeeATaskFromTheRows(t *testing.T) {
	f := newFreshWorld(t)
	for _, c := range freshCases {
		for _, r := range freshRoutes {
			t.Run(c.name+"/"+r.name, func(t *testing.T) {
				ids := f.chain(t, c.depth)
				task := ids[len(ids)-1]
				c.before(f, ids)
				f.gen()
				if !c.cold {
					seen, _ := f.call("ben")
					c.change(f, ids)
					after, n := f.call("ben")
					if n != 0 || after.Hides(task) != seen.Hides(task) {
						t.Fatalf("a read inside the window was not served from what was kept: %d decision reads, hides %v then %v", n, seen.Hides(task), after.Hides(task))
					}
				} else {
					c.change(f, ids)
				}
				inWindow := r.call(f, t, task)
				f.gen()
				fresh := r.call(f, t, task)
				ok := fresh >= 200 && fresh < 300
				if ok != c.allowed {
					t.Fatalf("with nothing kept, ben's %s answered %d; want allowed=%v", r.name, fresh, c.allowed)
				}
				if inWindow != fresh {
					t.Errorf("inside the window ben's %s answered %d, with nothing kept %d: the write was decided from a kept entry", r.name, inWindow, fresh)
				}
			})
		}
	}
}

// TestTheAccessCheckGrantsFromTheRows: the access check (WorkAccess
// FactsForMany, contract.go's through-a-task-above-it branch) on a ctx that
// carries the request's generation but is no read (a write, a worker, an
// agent's tool call), with Redis serving the generation and with Redis down,
// lets ben in only by what the rows say now.
func TestTheAccessCheckGrantsFromTheRows(t *testing.T) {
	f := newFreshWorld(t)
	for _, redisUp := range []bool{true, false} {
		t.Run(fmt.Sprintf("redis up %v", redisUp), func(t *testing.T) {
			ids := f.chain(t, 2)
			task := ids[1]
			f.restrict(ids[0], "", "ben")
			f.restrict(ids[1], ids[0])
			f.gen()
			if seen, _ := f.call("ben"); seen.Hides(task) {
				t.Fatal("ben, granted on the parent, does not see its task")
			}
			f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
			gen, err := redisInit.RedisClient.Get(context.Background(), "taskread:gen").Int64()
			if err != nil {
				t.Fatal(err)
			}
			ctx := taskReadGeneration.With(taskread.ActingFor(taskread.Install(context.Background()), f.ben), gen, redisUp)
			facts, err := workAccess.FactsForMany(ctx, f.ben, f.benUID, &dgraphStruct.DgraphTask{Uuid: task})
			if err != nil {
				t.Fatal(err)
			}
			if got := facts[strings.ToLower(task)]; !got.Restricted || got.Granted {
				t.Errorf("the access check, after ben's grant on the parent went: %+v; want restricted, not granted", got)
			}
		})
	}
}

// TestAnEventsListenersDecideFromTheRows: a GET may set off an event (the
// sign-in callbacks, router.go's /oauth_callback/{oauth_provider} and
// /oauth_callback/oidc: a first sign-in joins the default channels, and the
// join's event runs the workflows and rules, which make, move and restrict
// tasks). Its listeners get the request's ctx without its cancel, but they
// are writes, not the read: the access check in a listener goes by the rows,
// whatever the GET itself was served (its own read in the same window may
// still be the kept entry, H2) and whatever it had worked out already.
func TestAnEventsListenersDecideFromTheRows(t *testing.T) {
	f := newFreshWorld(t)
	marker := uuid.NewString()
	heard := make(chan workAccess.TaskFacts, 1)
	var task string
	webhookBusiness.RegisterEventListener(func(c context.Context, _ string, data map[string]interface{}) {
		if data["marker"] != marker {
			return
		}
		facts, err := workAccess.FactsForMany(c, f.ben, f.benUID, &dgraphStruct.DgraphTask{Uuid: task})
		if err != nil {
			t.Error(err)
		}
		heard <- facts[strings.ToLower(task)]
	})
	ids := f.chain(t, 2)
	task = ids[1]
	f.restrict(ids[0], "", "ben")
	f.restrict(ids[1], ids[0])
	f.gen()
	if seen, _ := f.call("ben"); seen.Hides(task) {
		t.Fatal("ben, granted on the parent, does not see its task")
	}
	f.exec(`DELETE FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, ids[0], f.people["ben"])
	gen, err := redisInit.RedisClient.Get(context.Background(), "taskread:gen").Int64()
	if err != nil {
		t.Fatal(err)
	}
	// The ctx a GET carries: middleware.RequestIDMiddleware marks it a read.
	get := taskReadGeneration.Cacheable(taskReadGeneration.With(taskread.ActingFor(taskread.Install(context.Background()), f.ben), gen, true))
	own, err := workAccess.FactsForMany(get, f.ben, f.benUID, &dgraphStruct.DgraphTask{Uuid: task})
	if err != nil {
		t.Fatal(err)
	}
	if got := own[strings.ToLower(task)]; !got.Granted {
		t.Fatalf("the GET's own read in the window: %+v; want the kept entry (granted), or the listener's answer proves nothing", got)
	}
	webhookBusiness.DispatchEvent(get, "h2wf.listener", map[string]interface{}{"marker": marker})
	select {
	case got := <-heard:
		if !got.Restricted || got.Granted {
			t.Errorf("the access check in a listener the GET set off, after ben's grant on the parent went: %+v; want restricted, not granted", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the listener never heard the event")
	}
}
