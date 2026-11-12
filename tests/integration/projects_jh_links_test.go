//go:build integration

package integration_test

// jira-01 J-H, the work hierarchy: epics and typed links. An epic is the
// seventh built-in task type on the one parent/sub-task relation
// (task_parent_task / task_sub_tasks), never a second hierarchy: epic ->
// task -> sub-task, in one project. An epic's tasks are top-level work: the
// backlog, the timeline and the plan read them as they read a task at the
// top; a task's sub-tasks stay under it.
// Run: integ_rel.sh <wt> <log> '^TestJHWorkHierarchyAndTypedLinks$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/searchprobe"
	"github.com/google/uuid"
)

// jhEpicType is the built-in type Epic (migration 582).
const jhEpicType = "0c06e5fa-fd1a-41d1-bb38-7cdb1fe8097e"

// jhOn points s's helpers at the subtest t, and back when it ends.
func jhOn(s *wa2Scale, t *testing.T) func() {
	was := s.t
	s.t = t
	return func() { s.t = was }
}

// jhCompact is a body as one line: the server writes JSON indented.
func jhCompact(body string) string {
	var flat bytes.Buffer
	if err := json.Compact(&flat, []byte(body)); err != nil {
		return body
	}
	return flat.String()
}

// jhUID is the graph uid of the task with uuid id.
func jhUID(s *wa2Scale, id string) string {
	s.t.Helper()
	var got struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(s.dg.Query(s.t, `{ q(func: eq(task_uuid, "`+id+`")) { uid } }`), &got); err != nil || len(got.Q) != 1 {
		s.t.Fatalf("task %s's uid: %v %+v", id, err, got)
	}
	return got.Q[0].UID
}

// jhEpic makes the task an epic, as its type is kept: in Postgres.
func jhEpic(s *wa2Scale, id string) {
	s.t.Helper()
	s.exec(`UPDATE tasks SET task_type_id = $1 WHERE id = $2`, jhEpicType, id)
}

// jhUnder puts child under parent on the one relation, as the app keeps it.
func jhUnder(s *wa2Scale, child, parent string) {
	s.t.Helper()
	c, p := jhUID(s, child), jhUID(s, parent)
	s.dg.Mutate(s.t, []map[string]any{
		{"uid": c, "task_parent_task": map[string]any{"uid": p}},
		{"uid": p, "task_sub_tasks": []map[string]any{{"uid": c}}},
	})
}

// jhUser is who as a request carries them.
func jhUser(s *wa2Scale, who string) *userModels.UserInfo {
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
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
}

// jhTree is the project's live tasks by uuid: name, parent's uuid and
// Postgres type's builtin ("" for none).
type jhNode struct{ Name, Parent, Builtin string }

func jhTree(s *wa2Scale) map[string]jhNode {
	s.t.Helper()
	var got struct {
		Q []struct {
			Tasks []struct {
				UUID   string `json:"task_uuid"`
				Name   string `json:"task_name"`
				Parent *struct {
					UUID string `json:"task_uuid"`
				} `json:"task_parent_task"`
			} `json:"project_tasks"`
		} `json:"q"`
	}
	q := `{ q(func: eq(project_uuid, "` + s.project + `")) { project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) (first: 1000) { task_uuid task_name task_parent_task { task_uuid } } } }`
	if err := json.Unmarshal(s.dg.Query(s.t, q), &got); err != nil || len(got.Q) != 1 {
		s.t.Fatalf("the project's tasks: %v", err)
	}
	out := map[string]jhNode{}
	for _, x := range got.Q[0].Tasks {
		n := jhNode{Name: x.Name}
		if x.Parent != nil {
			n.Parent = x.Parent.UUID
		}
		_ = s.env.PG.QueryRow(`SELECT COALESCE(tt.builtin, '') FROM tasks t LEFT JOIN task_types tt ON tt.id = t.task_type_id WHERE t.id = $1`, x.UUID).Scan(&n.Builtin)
		out[x.UUID] = n
	}
	return out
}

// jhPreds is the predicates the task with uuid id holds.
func jhPreds(s *wa2Scale, id string) map[string]bool {
	s.t.Helper()
	var got struct {
		Q []map[string]any `json:"q"`
	}
	if err := json.Unmarshal(s.dg.Query(s.t, `{ q(func: eq(task_uuid, "`+id+`")) { expand(_all_) { uid } } }`), &got); err != nil || len(got.Q) != 1 {
		s.t.Fatalf("task %s's predicates: %v", id, err)
	}
	out := map[string]bool{}
	for k := range got.Q[0] {
		out[k] = true
	}
	return out
}

// jhMove asks, as who, for task to go under parent ("" for the top).
func jhMove(s *wa2Scale, who, task, parent string) (int, string) {
	s.t.Helper()
	return s.postAs(who, taskController.SetTaskParent, map[string]string{"task_uuid": task, "parent_task_uuid": parent})
}

// jhSet sets one predicate of the node with uid.
func jhSet(s *wa2Scale, uid, pred string, v any) {
	s.t.Helper()
	s.dg.Mutate(s.t, []map[string]any{{"uid": uid, pred: v}})
}

// jhLink asks, as who, for task to link to other (remove: to take it off).
func jhLink(s *wa2Scale, who, task, other, kind, direction string, remove bool) (int, string) {
	s.t.Helper()
	h := taskController.LinkTasks
	if remove {
		h = taskController.UnlinkTasks
	}
	return s.postAs(who, h, map[string]string{"task_uuid": task, "other_task_uuid": other, "kind": kind, "direction": direction})
}

// jhLinks reads, as who, the task's links as the panel does.
func jhLinks(s *wa2Scale, who, task string) (int, string) {
	s.t.Helper()
	return s.call(who, http.MethodGet, "/", taskController.TaskLinks, map[string]string{"task_uuid": task}, nil)
}

// jhLinkNode is a TaskLink node as the graph keeps it.
type jhLinkNode struct {
	UID       string     `json:"uid"`
	Kind      string     `json:"tasklink_kind"`
	DeletedAt *time.Time `json:"tasklink_deleted_at"`
	DeletedBy *struct {
		UID string `json:"uid"`
	} `json:"tasklink_deleted_by"`
}

func (n jhLinkNode) live() bool { return n.DeletedAt == nil || n.DeletedAt.Year() <= 1970 }

// jhLinkNodes is every TaskLink node, live or not, from or to the task. The
// root asks for uid too: Dgraph leaves out a node none of whose asked fields
// it has, so a task with no links would read as no task.
func jhLinkNodes(s *wa2Scale, id string) []jhLinkNode {
	s.t.Helper()
	var got struct {
		Q []struct {
			Out []jhLinkNode `json:"outs"`
			In  []jhLinkNode `json:"ins"`
		} `json:"q"`
	}
	f := `uid tasklink_kind tasklink_deleted_at tasklink_deleted_by { uid }`
	if err := json.Unmarshal(s.dg.Query(s.t, `{ q(func: eq(task_uuid, "`+id+`")) { uid outs: ~tasklink_from { `+f+` } ins: ~tasklink_to { `+f+` } } }`), &got); err != nil || len(got.Q) != 1 {
		s.t.Fatalf("task %s's links: %v", id, err)
	}
	return append(got.Q[0].Out, got.Q[0].In...)
}

// jhCount is the count a links read answers.
func jhCount(s *wa2Scale, body string) int {
	s.t.Helper()
	var got struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		s.t.Fatalf("links' count: %v %s", err, jhCompact(body))
	}
	return got.Count
}

// jhRow is the first object in body (an answer, JSON) whose task_uuid is
// id: a task's row in a list, a board or a timeline, nil when none is.
func jhRow(body, id string) map[string]any {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return nil
	}
	var find func(v any) map[string]any
	find = func(v any) map[string]any {
		switch x := v.(type) {
		case map[string]any:
			if x["task_uuid"] == id {
				return x
			}
			for _, e := range x {
				if r := find(e); r != nil {
					return r
				}
			}
		case []any:
			for _, e := range x {
				if r := find(e); r != nil {
					return r
				}
			}
		}
		return nil
	}
	return find(v)
}

func TestJHWorkHierarchyAndTypedLinks(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.SetupRedis(t)
	// Search, for C1's search case; every case's writes index as the app's do.
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("connect search: %v", err)
	}
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.addTasks(12)
	// Statuses go round todo, inProgress, inReview, backlog, done, canceled.
	epic, inEpic, sub := s.tasks[0], s.tasks[1], s.tasks[3]
	jhEpic(s, epic)
	jhUnder(s, inEpic, epic)
	jhUnder(s, sub, inEpic)

	t.Run("an epic's tasks are top-level work", func(t *testing.T) {
		defer jhOn(s, t)()
		for name, h := range map[string]func() (int, string){
			"backlog":  func() (int, string) { return s.read("ana", cycleController.Backlog, "") },
			"timeline": func() (int, string) { return s.read("ana", projectController.ProjectTimeline, "") },
		} {
			code, out := h()
			if code != 200 {
				t.Fatalf("%s: %d %s", name, code, out)
			}
			if !strings.Contains(out, epic) || !strings.Contains(out, inEpic) {
				t.Errorf("%s leaves out the epic or its task: %s", name, jhCompact(out))
			}
			if strings.Contains(out, sub) {
				t.Errorf("%s reads a task's sub-task as top-level work: %s", name, jhCompact(out))
			}
		}
	})

	// The hierarchy's rules read each task's place in one query, its
	// children's counts summed at the parent (sum(val(...))): proved here
	// against the graph.
	t.Run("the hierarchy reads its places from the graph", func(t *testing.T) {
		defer jhOn(s, t)()
		got, err := taskDomain.ReadPlaces(context.Background(), epic, inEpic, sub)
		if err != nil {
			t.Fatalf("places: %v", err)
		}
		e, c, u := got[epic], got[inEpic], got[sub]
		if !e.Epic || e.Level() != 0 || e.SubTasks != 1 || !e.Deep || e.Parent != "" {
			t.Errorf("the epic: %+v", e)
		}
		if c.Level() != 1 || c.Parent != epic || !c.ParentEpic || c.SubTasks != 1 || c.Deep {
			t.Errorf("the epic's task: %+v", c)
		}
		if u.Level() != 2 || !u.SubTask() || u.Parent != inEpic {
			t.Errorf("the sub-task: %+v", u)
		}
		if err := taskDomain.MayParent(taskDomain.Place{UUID: "new", Project: u.Project}, &u); err == nil || !strings.Contains(err.Error(), "sub-task") {
			t.Errorf("a task went under a sub-task: %v", err)
		}
	})

	// C4: a task goes under an epic and out again on the one relation
	// (task_parent_task / task_sub_tasks), through the Task Writer, and
	// nothing else is written; an epic never goes under anything, and a
	// sub-task holds nothing.
	t.Run("a task goes under an epic and out again", func(t *testing.T) {
		defer jhOn(s, t)()
		moved := s.tasks[5]
		before := jhPreds(s, moved)
		if code, out := jhMove(s, "ana", moved, epic); code != 200 {
			t.Fatalf("under the epic: %d %s", code, jhCompact(out))
		}
		if got := jhTree(s)[moved].Parent; got != epic {
			t.Errorf("the task sits under %q, want the epic", got)
		}
		places, err := taskDomain.ReadPlaces(context.Background(), epic)
		if err != nil || places[epic].SubTasks != 2 {
			t.Errorf("the epic holds %d tasks, want 2: %v", places[epic].SubTasks, err)
		}
		for p := range jhPreds(s, moved) {
			if !before[p] && p != "task_parent_task" && p != "task_updated_at" && p != "task_activities" {
				t.Errorf("a move wrote %s: the hierarchy is the one relation (and its history line)", p)
			}
		}
		for _, c := range []struct{ name, task, parent, words string }{
			{"an epic under a task", epic, moved, "An epic sits at the top"},
			{"a task under a sub-task", s.tasks[7], sub, "A sub-task holds no tasks"},
			{"a task under itself", moved, moved, "under itself"},
		} {
			if code, out := jhMove(s, "ana", c.task, c.parent); code != 400 || !strings.Contains(out, c.words) {
				t.Errorf("%s: %d %s, want 400 saying %q", c.name, code, jhCompact(out), c.words)
			}
		}
		if code, out := jhMove(s, "ana", moved, ""); code != 200 {
			t.Fatalf("out to the top: %d %s", code, jhCompact(out))
		}
		if got := jhTree(s)[moved].Parent; got != "" {
			t.Errorf("the task still sits under %q", got)
		}
		places, err = taskDomain.ReadPlaces(context.Background(), epic)
		if err != nil || places[epic].SubTasks != 1 {
			t.Errorf("the epic holds %d tasks, want 1 again: %v", places[epic].SubTasks, err)
		}
	})

	// C2: a parent is only ever a live task the person may see, read by its
	// id in the change's own transaction (type, project, standing): a node
	// that isn't a task (even one carrying a task's id), a project, a
	// deleted task and a task kept from them are not found, and nothing is
	// written.
	t.Run("a parent that isn't a task the person sees is not found", func(t *testing.T) {
		defer jhOn(s, t)()
		moved := s.tasks[8]
		fake := uuid.NewString()
		s.dg.Mutate(t, []map[string]any{{"uid": "_:fake", "dgraph.type": "Doc", "task_uuid": fake, "task_name": "Not a task",
			"task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": s.uids["p"]}}})
		gone := s.tasks[9]
		jhSet(s, jhUID(s, gone), "task_deleted_at", time.Now().UTC().Format(time.RFC3339))
		defer jhSet(s, jhUID(s, gone), "task_deleted_at", "0001-01-01T00:00:00Z")
		cases := []struct{ name, who, parent string }{
			{"a node that isn't a task", "ana", fake},
			{"a project", "ana", s.project},
			{"a deleted task", "ana", gone},
		}
		if s.restrict(1) {
			defer s.restrict(0)
			cases = append(cases, struct{ name, who, parent string }{"a task kept from them", "ben", s.tasks[2]})
		}
		for _, c := range cases {
			code, out := jhMove(s, c.who, moved, c.parent)
			if code != 404 || strings.Contains(out, c.parent) {
				t.Errorf("%s: %d %s, want 404 naming nothing", c.name, code, jhCompact(out))
			}
			if got := jhTree(s)[moved].Parent; got != "" {
				t.Errorf("%s: the task went under %q", c.name, got)
			}
		}
	})

	// C3: removing a link is soft: the node stays, stamped with when and by
	// whom; adding it again revives the same node. relates is one node per
	// pair, whichever side adds it; a directed link reads from each side.
	t.Run("removing a link is soft, and adding it again revives it", func(t *testing.T) {
		defer jhOn(s, t)()
		a, b, c := s.tasks[4], s.tasks[6], s.tasks[10]
		for _, side := range [][2]string{{a, b}, {b, a}} {
			if code, out := jhLink(s, "ana", side[0], side[1], "relates", "", false); code != 200 {
				t.Fatalf("relates: %d %s", code, jhCompact(out))
			}
		}
		nodes := jhLinkNodes(s, a)
		if len(nodes) != 1 || !nodes[0].live() || nodes[0].Kind != "relates" {
			t.Fatalf("one pair, two adds: %+v, want one live node", nodes)
		}
		first := nodes[0].UID
		if code, out := jhLinks(s, "ana", b); code != 200 || !strings.Contains(out, a) || !strings.Contains(jhCompact(out), `"label":"relates to"`) {
			t.Errorf("b's links: %d %s", code, jhCompact(out))
		}
		if code, out := jhLink(s, "ana", a, b, "relates", "", true); code != 200 {
			t.Fatalf("unlink: %d %s", code, jhCompact(out))
		}
		nodes = jhLinkNodes(s, a)
		if len(nodes) != 1 || nodes[0].UID != first || nodes[0].live() || nodes[0].DeletedBy == nil || nodes[0].DeletedBy.UID != s.uids["ana"] {
			t.Errorf("removed: %+v, want the same node stamped as removed by ana", nodes)
		}
		if _, out := jhLinks(s, "ana", a); strings.Contains(out, b) {
			t.Errorf("a removed link is still read: %s", jhCompact(out))
		}
		if code, out := jhLink(s, "ana", b, a, "relates", "", false); code != 200 {
			t.Fatalf("again: %d %s", code, jhCompact(out))
		}
		nodes = jhLinkNodes(s, a)
		if len(nodes) != 1 || nodes[0].UID != first || !nodes[0].live() || nodes[0].DeletedBy != nil {
			t.Errorf("added again: %+v, want the same node, live", nodes)
		}
		if _, out := jhLinks(s, "ana", a); !strings.Contains(out, b) {
			t.Errorf("a link added again isn't read: %s", jhCompact(out))
		}
		if code, out := jhLink(s, "ana", a, c, "duplicates", "", false); code != 200 {
			t.Fatalf("duplicates: %d %s", code, jhCompact(out))
		}
		if _, out := jhLinks(s, "ana", a); !strings.Contains(jhCompact(out), `"label":"duplicates"`) {
			t.Errorf("a's side: %s", jhCompact(out))
		}
		if _, out := jhLinks(s, "ana", c); !strings.Contains(jhCompact(out), `"label":"is duplicated by"`) {
			t.Errorf("c's side: %s", jhCompact(out))
		}
	})

	// C5: blocking is the date dependency (task_blocked_by, finish to start
	// and the rest), never a link: "blocks" is refused in words pointing
	// there, and nothing is written.
	t.Run("blocking is a dependency, never a link", func(t *testing.T) {
		defer jhOn(s, t)()
		a, b := s.tasks[4], s.tasks[8]
		before := len(jhLinkNodes(s, a))
		code, out := jhLink(s, "ana", a, b, "blocks", "", false)
		if code != 400 || !strings.Contains(out, "Dependencies") {
			t.Errorf("blocks: %d %s, want 400 pointing to dependencies", code, jhCompact(out))
		}
		if got := len(jhLinkNodes(s, a)); got != before {
			t.Errorf("a refused link wrote %d nodes", got-before)
		}
		if jhPreds(s, a)["task_blocked_by"] {
			t.Errorf("a refused link made a dependency")
		}
	})

	// C2: a link's other end is only ever a live task the person sees, read
	// by its id in the change's own transaction: a node that isn't a task, a
	// project, a deleted task and a task kept from them are not found, and
	// nothing is written. A link is changed only between its own two tasks.
	t.Run("a link's other end is a task the person sees", func(t *testing.T) {
		defer jhOn(s, t)()
		a := s.tasks[8]
		fake := uuid.NewString()
		s.dg.Mutate(t, []map[string]any{{"uid": "_:fake", "dgraph.type": "Doc", "task_uuid": fake, "task_name": "Not a task",
			"task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": s.uids["p"]}}})
		gone := s.tasks[9]
		jhSet(s, jhUID(s, gone), "task_deleted_at", time.Now().UTC().Format(time.RFC3339))
		defer jhSet(s, jhUID(s, gone), "task_deleted_at", "0001-01-01T00:00:00Z")
		cases := []struct{ name, who, other string }{
			{"a node that isn't a task", "ana", fake},
			{"a project", "ana", s.project},
			{"a deleted task", "ana", gone},
		}
		if s.restrict(1) {
			defer s.restrict(0)
			cases = append(cases, struct{ name, who, other string }{"a task kept from them", "ben", s.tasks[2]})
		}
		for _, c := range cases {
			code, out := jhLink(s, c.who, a, c.other, "relates", "", false)
			if code != 404 || strings.Contains(out, c.other) {
				t.Errorf("%s: %d %s, want 404 naming nothing", c.name, code, jhCompact(out))
			}
		}
		if n := jhLinkNodes(s, a); len(n) != 0 {
			t.Errorf("refused links wrote %+v", n)
		}
		// The pair 4-6 is linked; taking off "8 relates 6" leaves it.
		if code, out := jhLink(s, "ana", a, s.tasks[6], "relates", "", true); code != 200 {
			t.Errorf("taking off a link that isn't there: %d %s", code, jhCompact(out))
		}
		for _, n := range jhLinkNodes(s, s.tasks[6]) {
			if !n.live() {
				t.Errorf("another pair's link was removed: %+v", n)
			}
		}
	})

	// C1 (the panel): a link to a task the reader can't see reveals nothing:
	// not its id, name or key, and it isn't counted.
	t.Run("a link to a task the reader can't see reveals nothing", func(t *testing.T) {
		defer jhOn(s, t)()
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		a, hidden := s.tasks[4], s.tasks[2]
		if code, out := jhLink(s, "ana", a, hidden, "relates", "", false); code != 200 {
			t.Fatalf("ana links the restricted task: %d %s", code, jhCompact(out))
		}
		_, all := jhLinks(s, "ana", a)
		code, out := jhLinks(s, "ben", a)
		if code != 200 {
			t.Fatalf("ben reads the links: %d %s", code, jhCompact(out))
		}
		if strings.Contains(out, hidden) || strings.Contains(out, `"Task 2"`) {
			t.Errorf("ben reads the task kept from him: %s", jhCompact(out))
		}
		if got, want := jhCount(s, out), jhCount(s, all)-1; got != want {
			t.Errorf("ben counts %d links, want %d", got, want)
		}
		if code, out := jhLinks(s, "ben", hidden); code != 404 {
			t.Errorf("ben reads the hidden task's own links: %d %s", code, jhCompact(out))
		}
	})

	// C1 (the API and MCP): the same read through a person's token, on GET
	// /v1/tasks/{id}/links and the MCP tool tasks_links (one op): a link to a
	// task the reader can't see reveals nothing, and that task's own links
	// aren't found.
	t.Run("the API and MCP read a link to a hidden task as nothing", func(t *testing.T) {
		defer jhOn(s, t)()
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		t.Setenv("JWT_SECRET", "jh-links-integration-secret")
		t.Setenv("ALLOWED_USERS", "")
		ctx := context.Background()
		settingsBusiness.ForgetSettingsForTest()
		t.Cleanup(settingsBusiness.ForgetSettingsForTest)
		if err := mcpBusiness.SetAdmission(ctx, true, "*"); err != nil {
			t.Fatalf("turn MCP on: %v", err)
		}
		for _, register := range []func() error{mcpBusiness.RegisterBridgedTools, mcpBusiness.PublishOps} {
			if err := register(); err != nil && !strings.Contains(err.Error(), "already registered") {
				t.Fatalf("register the MCP tools: %v", err)
			}
		}
		a, hidden, seen := s.tasks[7], s.tasks[2], s.tasks[9]
		for _, other := range []string{hidden, seen} {
			if code, out := jhLink(s, "ana", a, other, "relates", "", false); code != 200 {
				t.Fatalf("ana links %s: %d %s", other, code, jhCompact(out))
			}
		}
		tok, err := apiTokenBusiness.CreateToken(ctx, "ben's script", []string{apiTokenBusiness.ScopeTasksRead}, 30, s.people["ben"], nil, false)
		if err != nil {
			t.Fatalf("ben's token: %v", err)
		}
		routes := router.Routes()
		send := func(method, path string, body any) (int, string) {
			t.Helper()
			var rd io.Reader
			if body != nil {
				raw, _ := json.Marshal(body)
				rd = bytes.NewReader(raw)
			}
			req := httptest.NewRequest(method, path, rd)
			req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			return rec.Code, rec.Body.String()
		}
		tool := func(id string) (int, string) {
			t.Helper()
			return send(http.MethodPost, "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "tasks_links", "arguments": map[string]any{"id": id}}})
		}
		reveals := func(body string) bool { return strings.Contains(body, hidden) || strings.Contains(body, "Task 2") }

		code, out := send(http.MethodGet, "/v1/tasks/"+a+"/links", nil)
		if code != 200 || !strings.Contains(out, seen) {
			t.Fatalf("ben reads the links over the API: %d %s", code, jhCompact(out))
		}
		if reveals(out) || jhCount(s, out) != 1 {
			t.Errorf("ben's API read names or counts the task kept from him: %s", jhCompact(out))
		}
		if code, out := send(http.MethodGet, "/v1/tasks/"+hidden+"/links", nil); code != 404 || reveals(out) {
			t.Errorf("ben reads the hidden task's own links over the API: %d %s", code, jhCompact(out))
		}
		code, out = tool(a)
		if code != 200 || !strings.Contains(out, seen) || strings.Contains(jhCompact(out), `"isError":true`) {
			t.Fatalf("ben reads the links over MCP: %d %s", code, jhCompact(out))
		}
		if reveals(out) {
			t.Errorf("ben's MCP read names the task kept from him: %s", jhCompact(out))
		}
		code, out = tool(hidden)
		if flat := jhCompact(out); code == 200 && !strings.Contains(flat, `"isError":true`) && !strings.Contains(flat, `"error":`) || reveals(out) {
			t.Errorf("ben reads the hidden task's own links over MCP: %d %s", code, flat)
		}
		// By key, as REST takes it (serveLinks): MCP's check of the task
		// (reach.go ResourceTask) reads the same key, and the hidden task's
		// key is answered as a task that isn't there.
		if err := taskKeys.Settle(ctx, uuid.MustParse(s.project)); err != nil {
			t.Fatalf("number the project's tasks: %v", err)
		}
		key, hiddenKey := taskKeys.KeyOf(ctx, uuid.MustParse(a)), taskKeys.KeyOf(ctx, uuid.MustParse(hidden))
		if key == "" || hiddenKey == "" {
			t.Fatalf("the tasks have no keys (%q, %q), so this proves nothing", key, hiddenKey)
		}
		code, out = tool(key)
		if flat := jhCompact(out); code != 200 || !strings.Contains(out, seen) || strings.Contains(flat, `"isError":true`) || reveals(out) {
			t.Errorf("ben reads the links over MCP by the task's key %s: %d %s", key, code, flat)
		}
		code, out = tool(hiddenKey)
		if flat := jhCompact(out); code == 200 && !strings.Contains(flat, `"isError":true`) && !strings.Contains(flat, `"error":`) || reveals(out) {
			t.Errorf("ben reads the hidden task's own links over MCP by its key: %d %s", code, flat)
		}
	})

	// C1 (search): a link isn't indexed, and a task's search entry carries
	// nothing of the tasks it links to, so search never finds a task kept from
	// the reader through a link: ana finds the restricted task by its name; ben
	// doesn't, and his hit for the task linked to it names nothing of it.
	t.Run("search never finds a hidden task through a link", func(t *testing.T) {
		defer jhOn(s, t)()
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		a, hidden := s.tasks[10], s.tasks[2]
		if code, out := jhLink(s, "ana", a, hidden, "duplicates", "", false); code != 200 {
			t.Fatalf("ana links the restricted task: %d %s", code, jhCompact(out))
		}
		ctx := context.Background()
		var docs []*openSearchStruct.OpenSearchTask
		for id, name := range map[string]string{a: "Task 10", hidden: "Task 2"} {
			docs = append(docs, &openSearchStruct.OpenSearchTask{Uuid: id, TaskName: name, TaskProjectUuid: s.project, TaskStatus: "todo",
				TaskCreatedAt: time.Now().Unix(), TaskCreatedByUserUuid: s.people["ana"].String()})
		}
		taskDomain.CreateTasksInOpenSearch(ctx, docs)
		as := func(who, text string) string {
			t.Helper()
			id := s.people[who].String()
			r := searchprobe.SearchAs(t, taskread.ActingFor(ctx, id), globalSearchBusiness.Reach{UserUUID: id, Email: id + "@example.test",
				Projects: []string{s.project}}, text, globalSearchBusiness.Page{Kinds: []string{globalSearchBusiness.KindTask}})
			raw, _ := json.Marshal(r.Page)
			return string(raw)
		}
		found := ""
		for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && !strings.Contains(found, hidden); time.Sleep(300 * time.Millisecond) {
			found = as("ana", "Task 2")
		}
		if !strings.Contains(found, hidden) {
			t.Fatalf("ana's search never finds the restricted task, so this proves nothing: %s", found)
		}
		if got := as("ben", "Task 2"); strings.Contains(got, hidden) || strings.Contains(got, `"Task 2"`) {
			t.Errorf("ben's search finds the task kept from him: %s", got)
		}
		got := as("ben", "Task 10")
		if !strings.Contains(got, a) {
			t.Errorf("ben's search doesn't find the task he may see: %s", got)
		}
		if strings.Contains(got, hidden) || strings.Contains(got, `"Task 2"`) {
			t.Errorf("ben's hit for the linked task names the one kept from him: %s", got)
		}
	})

	// A link and a move are lines in the history of every task they touch,
	// written in the change's own mutation: both ends of a link name each
	// other; the moved task names its parents, the epic it went under names
	// it by name and, once it leaves, by id. A line naming a task kept from
	// the reader keeps what happened and loses which task.
	t.Run("a link and a move are history lines on every task they touch", func(t *testing.T) {
		defer jhOn(s, t)()
		history := func(who, task string) string {
			t.Helper()
			code, out := s.call(who, http.MethodGet, "/", taskController.GetTaskActivityList, map[string]string{"task_uuid": task}, nil)
			if code != 200 {
				t.Fatalf("%s reads the history of %s: %d %s", who, task, code, jhCompact(out))
			}
			return jhCompact(out)
		}
		// A line of kind naming next ("": naming nothing), as the read
		// writes it: type, time, then the state.
		line := func(kind, next string) *regexp.Regexp {
			tail := `\}`
			if next != "" {
				tail = `,"activity_next_state":"` + regexp.QuoteMeta(next) + `"`
			}
			return regexp.MustCompile(`"activity_type":"` + kind + `","activity_time":"[^"]*"` + tail)
		}
		a, b := s.tasks[6], s.tasks[11]
		for _, remove := range []bool{false, true} {
			if code, out := jhLink(s, "ana", a, b, "clones", "", remove); code != 200 {
				t.Fatalf("ana links (remove %v): %d %s", remove, code, jhCompact(out))
			}
			kind := dgraphStruct.ACTIVITY_TYPE_ADD_LINK
			if remove {
				kind = dgraphStruct.ACTIVITY_TYPE_REMOVE_LINK
			}
			for task, other := range map[string]string{a: b, b: a} {
				if h := history("ana", task); !line(kind, other).MatchString(h) {
					t.Errorf("%s's history has no %s line naming %s: %s", task, kind, other, h)
				}
			}
		}
		if code, out := jhMove(s, "ana", a, epic); code != 200 {
			t.Fatalf("under the epic: %d %s", code, jhCompact(out))
		}
		if code, out := jhMove(s, "ana", a, ""); code != 200 {
			t.Fatalf("out to the top: %d %s", code, jhCompact(out))
		}
		h := history("ana", a)
		for _, l := range []*regexp.Regexp{line(dgraphStruct.ACTIVITY_TYPE_PARENT, epic), line(dgraphStruct.ACTIVITY_TYPE_PARENT, "")} {
			if !l.MatchString(h) {
				t.Errorf("the moved task's history has no %s: %s", l, h)
			}
		}
		h = history("ana", epic)
		for _, l := range []*regexp.Regexp{line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "Task 6"), line(dgraphStruct.ACTIVITY_TYPE_REMOVE_SUB_TASK, a)} {
			if !l.MatchString(h) {
				t.Errorf("the epic's history has no %s: %s", l, h)
			}
		}
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		hidden := s.tasks[2]
		if code, out := jhLink(s, "ana", a, hidden, "clones", "", false); code != 200 {
			t.Fatalf("ana links the restricted task: %d %s", code, jhCompact(out))
		}
		defer jhLink(s, "ana", a, hidden, "clones", "", true)
		if h := history("ana", a); !line(dgraphStruct.ACTIVITY_TYPE_ADD_LINK, hidden).MatchString(h) {
			t.Fatalf("ana's history doesn't name the restricted task, so this proves nothing: %s", h)
		}
		h = history("ben", a)
		if strings.Contains(h, hidden) || strings.Contains(h, `"Task 2"`) {
			t.Errorf("ben's history names the task kept from him: %s", h)
		}
		if !line(dgraphStruct.ACTIVITY_TYPE_ADD_LINK, "").MatchString(h) {
			t.Errorf("ben's history lost the line itself: %s", h)
		}
	})

	// C1 (history): a task's history names nothing of a task kept from the
	// reader. A dependency's line keeps the blocker's id (recordDependency),
	// and the task read and its history page answered it to anyone who reads
	// the task: ana sees the line name the restricted blocker; ben, who may
	// not see it, reads the same task and its history without its id or name.
	t.Run("a task's history names nothing of a task kept from the reader", func(t *testing.T) {
		defer jhOn(s, t)()
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		a, hidden := s.tasks[7], s.tasks[2]
		dep := map[string]string{"task_uuid": a, "blocked_by_uuid": hidden}
		if code, out := s.postAs("ana", taskController.AddTaskDependency, dep); code != 200 {
			t.Fatalf("ana makes the task wait on the restricted one: %d %s", code, jhCompact(out))
		}
		defer s.postAs("ana", taskController.RemoveTaskDependency, dep)
		read := func(who string, h http.HandlerFunc) (int, string) {
			t.Helper()
			return s.call(who, http.MethodGet, "/", h, map[string]string{"task_uuid": a}, nil)
		}
		if _, out := read("ana", taskController.GetTaskActivityList); !strings.Contains(out, hidden) {
			t.Fatalf("ana's history of the task doesn't name its blocker, so this proves nothing: %s", jhCompact(out))
		}
		for name, h := range map[string]http.HandlerFunc{"the task": taskController.GetTaskInfo, "its history": taskController.GetTaskActivityList} {
			code, out := read("ben", h)
			if code != 200 {
				t.Fatalf("ben reads %s: %d %s", name, code, jhCompact(out))
			}
			if strings.Contains(out, hidden) || strings.Contains(out, `"Task 2"`) {
				t.Errorf("ben reads %s and it names the task kept from him: %s", name, jhCompact(out))
			}
		}
	})

	// A task holds at most 200 live links (both ways): the next is refused
	// in words, and a read stops at 200.
	t.Run("a task holds at most 200 links", func(t *testing.T) {
		defer jhOn(s, t)()
		full, to := s.tasks[11], s.tasks[5]
		fu, tu := jhUID(s, full), jhUID(s, to)
		var nodes []map[string]any
		for i := 0; i < 200; i++ {
			nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:l%d", i), "dgraph.type": "TaskLink", "tasklink_key": fmt.Sprintf("clones:%s:%d", full, i),
				"tasklink_kind": "clones", "tasklink_from": map[string]any{"uid": fu}, "tasklink_to": map[string]any{"uid": tu},
				"tasklink_deleted_at": "0001-01-01T00:00:00Z"})
		}
		made := s.dg.Mutate(t, nodes)
		defer func() {
			var gone []map[string]any
			for _, uid := range made {
				gone = append(gone, map[string]any{"uid": uid, "tasklink_deleted_at": time.Now().UTC().Format(time.RFC3339)})
			}
			s.dg.Mutate(t, gone)
		}()
		if code, out := jhLink(s, "ana", full, s.tasks[7], "relates", "", false); code != 400 || !strings.Contains(out, "200") {
			t.Errorf("the 201st link: %d %s, want 400 naming the cap", code, jhCompact(out))
		}
		if code, out := jhLinks(s, "ana", full); code != 200 || jhCount(s, out) != 200 {
			t.Errorf("a full task's links: %d, count %d", code, jhCount(s, out))
		}
	})

	// An archived project's tasks are read-only: moving or linking one is
	// refused in words.
	t.Run("an archived project's tasks are read-only", func(t *testing.T) {
		defer jhOn(s, t)()
		jhSet(s, s.uids["p"], "project_deleted_at", time.Now().UTC().Format(time.RFC3339))
		// Defers run last first: the project comes back, then a move it took
		// wrongly is undone, so no later subtest counts that task in the epic.
		defer jhMove(s, "ana", s.tasks[5], "")
		defer jhSet(s, s.uids["p"], "project_deleted_at", "0001-01-01T00:00:00Z")
		if code, out := jhMove(s, "ana", s.tasks[5], epic); code != 403 || !strings.Contains(out, `"msg"`) {
			t.Errorf("a move: %d %s, want 403 in words", code, jhCompact(out))
		}
		if code, out := jhLink(s, "ana", s.tasks[5], s.tasks[7], "relates", "", false); code != 403 || !strings.Contains(out, `"msg"`) {
			t.Errorf("a link: %d %s, want 403 in words", code, jhCompact(out))
		}
		// Taking one out to the top, or a dependency off, changes the task too.
		if code, out := jhMove(s, "ana", s.tasks[5], ""); code != 403 || !strings.Contains(out, "archived") {
			t.Errorf("a move out: %d %s, want 403 in words", code, jhCompact(out))
		}
		dep := map[string]string{"task_uuid": s.tasks[5], "blocked_by_uuid": s.tasks[7]}
		if code, out := s.postAs("ana", taskController.RemoveTaskDependency, dep); code != 403 || !strings.Contains(out, "archived") {
			t.Errorf("a dependency off: %d %s, want 403 in words", code, jhCompact(out))
		}
	})

	// Piece 8: an epic is a container. Its progress (its tasks, and how many
	// are closed) counts only the tasks the reader may see, on the list, the
	// board and the timeline; and its own estimate and dates count in no plan
	// total, while its tasks' do.
	t.Run("an epic's progress and the plan count its work, not the epic", func(t *testing.T) {
		defer jhOn(s, t)()
		if !s.restrict(1) {
			t.Skip("no restricted tasks on this server")
		}
		defer s.restrict(0)
		hidden, closed := s.tasks[2], s.tasks[4] // restricted, in review; done
		for _, c := range []string{hidden, closed} {
			if code, out := jhMove(s, "ana", c, epic); code != 200 {
				t.Fatalf("ana puts %s under the epic: %d %s", c, code, jhCompact(out))
			}
			defer jhMove(s, "ana", c, "")
		}
		project := map[string]string{"project_uuid": s.project}
		reads := map[string]http.HandlerFunc{"the list": projectController.GetProjectTaskList,
			"the board": projectController.GetProjectTaskListForKanban, "the timeline": projectController.ProjectTimeline}
		for who, want := range map[string][2]float64{"ana": {3, 1}, "ben": {2, 1}} {
			for name, h := range reads {
				code, out := s.call(who, http.MethodGet, "/?getAll=1", h, project, nil)
				row := jhRow(out, epic)
				if code != 200 || row == nil {
					t.Fatalf("%s reads %s: %d, no epic row: %s", who, name, code, jhCompact(out))
				}
				if n, d := row["task_sub_task_count"], row["task_sub_task_done"]; n != want[0] || d != want[1] {
					t.Errorf("%s reads %s: the epic counts %v tasks, %v closed; want %v", who, name, n, d, want)
				}
			}
		}
		jhSet(s, jhUID(s, epic), "task_estimate_points", 977)
		defer jhSet(s, jhUID(s, epic), "task_estimate_points", 0)
		asAna := taskread.ActingFor(taskread.Install(context.Background()), s.people["ana"].String())
		now := time.Now().UTC()
		totals, err := taskDomain.ReadPlanTotalsOf(asAna, []string{s.project}, []time.Time{now, now.AddDate(0, 0, -7)})
		if err != nil {
			t.Fatalf("the plan's totals: %v", err)
		}
		if got := totals[s.project]; got.Open.Points >= 977 || got.Open.Pointed > 0 {
			t.Errorf("the plan's open work counts the epic's own estimate: %+v", got.Open)
		}
		plan, err := taskDomain.PlanTasksOf(asAna, []string{s.project}, s.people["ana"].String(), taskDomain.PlanLimits{Finished: 50})
		if err != nil {
			t.Fatalf("the plan's tasks: %v", err)
		}
		inPlan := map[string]bool{}
		for _, pt := range plan[s.project].Tasks {
			inPlan[pt.Uuid] = true
		}
		if inPlan[epic] || !inPlan[inEpic] || !inPlan[closed] {
			t.Errorf("the plan holds the epic (%v), or leaves out its tasks (%v %v)", inPlan[epic], inPlan[inEpic], inPlan[closed])
		}
		// The people workload is a total too: the epic (open, dated, ben's,
		// with its own estimate) weighs on no one; its dated open task does.
		jhSet(s, jhUID(s, epic), "task_estimate_minutes", 977)
		defer jhSet(s, jhUID(s, epic), "task_estimate_minutes", 0)
		code, out := s.call("ana", http.MethodGet, "/?weeks=2", projectController.Workload, nil, nil)
		if code != 200 || !strings.Contains(out, hidden) {
			t.Fatalf("ana's workload doesn't hold the epic's task, so this proves nothing: %d %s", code, jhCompact(out))
		}
		if strings.Contains(out, epic) {
			t.Errorf("ana's workload counts the epic itself: %s", jhCompact(out))
		}
	})

	t.Run("a template copies the whole hierarchy", func(t *testing.T) {
		defer jhOn(s, t)()
		ctx := context.Background()
		tpl, err := projectTemplate.Snapshot(ctx, uuidOf(t, s.project), "Copy", "", time.UTC)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		before := jhTree(s)
		applied := projectTemplate.ApplyTo(ctx, tpl, s.project, jhUser(s, "ana"), projectTemplate.Start{Day: time.Now().UTC(), Loc: time.UTC})
		if applied.Failed != 0 {
			t.Fatalf("apply: %+v", applied)
		}
		after := jhTree(s)
		var newEpic, newChild, newSub string
		for id, n := range after {
			if _, old := before[id]; old {
				continue
			}
			switch n.Name {
			case "Task 0":
				newEpic = id
			case "Task 1":
				newChild = id
			case "Task 3":
				newSub = id
			}
		}
		if newEpic == "" || after[newEpic].Builtin != "epic" || after[newEpic].Parent != "" {
			t.Fatalf("the copy's epic: %q %+v", newEpic, after[newEpic])
		}
		if after[newChild].Parent != newEpic {
			t.Errorf("the copy's epic doesn't hold its task: %+v", after[newChild])
		}
		if after[newSub].Parent != newChild || newChild == "" {
			t.Errorf("the copy's task doesn't hold its sub-task: %+v", after[newSub])
		}
	})
}
