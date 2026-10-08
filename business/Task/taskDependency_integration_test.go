//go:build integration

package business

// Task dependencies against a real Postgres and Dgraph with the production schema.
// Run: go test -tags=integration ./business/Task/ -run TestTaskDependencies -v

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestTaskDependencies(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	live := "0001-01-01T00:00:00Z"
	project, other := uuid.NewString(), uuid.NewString()
	id := map[string]string{}
	for _, k := range []string{"a", "b", "c", "s", "x", "y", "gone"} {
		id[k] = uuid.NewString()
	}
	task := func(key, name string, more map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id[key], "task_name": name,
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:p"}}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:p", "dgraph.type": "Project", "project_uuid": project,
		"project_admins":  []map[string]any{{"uid": "_:admin", "dgraph.type": "User", "user_uuid": uuid.NewString()}},
		"project_members": []map[string]any{{"uid": "_:member", "dgraph.type": "User", "user_uuid": uuid.NewString()}},
		"project_tasks": []map[string]any{
			task("a", "Design", map[string]any{"task_due_date": "2026-03-04T17:00:00Z"}),
			task("b", "Build", map[string]any{"task_start_date": "2026-03-05T09:00:00Z", "task_due_date": "2026-03-07T17:00:00Z"}),
			task("c", "Ship", nil),
			task("gone", "Old", map[string]any{"task_deleted_at": "2026-03-01T00:00:00Z"}),
		},
	})
	dg.Mutate(t, map[string]any{"uid": uids["p"], "project_tasks": []map[string]any{
		{"uid": "_:s", "dgraph.type": "Task", "task_uuid": id["s"], "task_name": "step", "task_status": "todo", "task_deleted_at": live,
			"task_project": map[string]any{"uid": uids["p"]}, "task_parent_task": map[string]any{"uid": uids["a"]}},
	}})
	// x is another project's; y is too, though it waits on Design.
	dg.Mutate(t, map[string]any{"uid": "_:q", "dgraph.type": "Project", "project_uuid": other,
		"project_admins": []map[string]any{{"uid": uids["admin"]}},
		"project_tasks": []map[string]any{
			{"uid": "_:x", "dgraph.type": "Task", "task_uuid": id["x"], "task_name": "Elsewhere",
				"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:q"}},
			{"uid": "_:y", "dgraph.type": "Task", "task_uuid": id["y"], "task_name": "Moved away", "task_status": "todo",
				"task_deleted_at": live, "task_project": map[string]any{"uid": "_:q"}, "task_start_date": "2026-03-05T09:00:00Z",
				"task_blocked_by": []map[string]any{{"uid": uids["a"]}}},
		}})
	admin := &dgraphStruct.DgraphUser{Uid: uids["admin"]}
	member := &dgraphStruct.DgraphUser{Uid: uids["member"]}

	read := func(key string) (blockedBy []string, start, due time.Time, lines int) {
		t.Helper()
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
			`query q($id: string) { t(func: eq(task_uuid, $id)) { task_blocked_by { task_uuid } task_start_date task_due_date n: count(task_activities) } }`,
			map[string]string{"$id": id[key]})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			T []struct {
				By []struct {
					UUID string `json:"task_uuid"`
				} `json:"task_blocked_by"`
				Start time.Time `json:"task_start_date"`
				Due   time.Time `json:"task_due_date"`
				N     int       `json:"n"`
			}
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read back %s: %v %s", key, err, resp.Json)
		}
		for _, b := range out.T[0].By {
			blockedBy = append(blockedBy, b.UUID)
		}
		return blockedBy, out.T[0].Start, out.T[0].Due, out.T[0].N
	}

	// how reads the facets of key's link to on: its kind ("" when it has
	// none) and lag, and whether it's there.
	how := func(key, on string) (kind string, lag int, there bool) {
		t.Helper()
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
			`query q($id: string, $on: string) { t(func: eq(task_uuid, $id)) { task_blocked_by @facets(kind, lag) @filter(eq(task_uuid, $on)) { task_uuid } } }`,
			map[string]string{"$id": id[key], "$on": id[on]})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			T []struct {
				By []dgraphStruct.DgraphTask `json:"task_blocked_by"`
			}
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.T) != 1 || len(out.T[0].By) != 1 {
			return "", 0, false
		}
		return out.T[0].By[0].DependencyKind, out.T[0].By[0].DependencyLag, true
	}

	if err := AddTaskDependency(ctx, id["b"], id["a"], "", 0, admin); err != nil {
		t.Fatal(err)
	}
	if by, _, _, lines := read("b"); len(by) != 1 || by[0] != id["a"] || lines != 1 {
		t.Fatalf("Build waits on Design, with a line in its history: %v %d", by, lines)
	}
	if kind, lag, _ := how("b", "a"); kind != FinishToStart || lag != 0 {
		t.Fatalf("no kind is finish to start, with no lag: %q %d", kind, lag)
	}
	if err := AddTaskDependency(ctx, id["b"], id["a"], FinishToStart, 0, admin); err != nil {
		t.Fatal(err)
	}
	if by, _, _, lines := read("b"); len(by) != 1 || lines != 1 {
		t.Fatalf("adding it again changes nothing, nor writes another line: %v %d", by, lines)
	}
	// Changing how it waits keeps one link, and the history its one line.
	if err := AddTaskDependency(ctx, id["b"], id["a"], StartToStart, -2, admin); err != nil {
		t.Fatal(err)
	}
	if kind, lag, _ := how("b", "a"); kind != StartToStart || lag != -2 {
		t.Fatalf("Build now starts two days before Design does, at the earliest: %q %d", kind, lag)
	}
	if by, _, _, lines := read("b"); len(by) != 1 || lines != 1 {
		t.Fatalf("a change of kind is no new dependency: %v %d", by, lines)
	}
	if err := AddTaskDependency(ctx, id["b"], id["a"], FinishToStart, 0, admin); err != nil {
		t.Fatal(err)
	}
	if kind, lag, _ := how("b", "a"); kind != FinishToStart || lag != 0 {
		t.Fatalf("back to finish to start, and the lag goes with the old kind: %q %d", kind, lag)
	}
	for _, c := range []struct {
		task, on string
		kind     string
		lag      int
		user     *dgraphStruct.DgraphUser
		want     error
	}{
		{"b", "b", "", 0, admin, ErrDependencySelf},
		{"a", "b", "", 0, admin, ErrDependencyLoop},
		{"a", "b", FinishToFinish, 3, admin, ErrDependencyLoop},
		{"b", "x", "", 0, admin, ErrDependencyProject},
		{"s", "a", "", 0, admin, ErrDependencySubtask},
		{"c", "a", "", 0, member, ErrNotProjectAdmin},
		{"c", "gone", "", 0, admin, ErrDependencyMissing},
		{"c", "a", "xx", 0, admin, ErrDependencyKind},
		{"c", "a", StartToStart, MaxLag + 1, admin, ErrDependencyKind},
		{"c", "a", StartToStart, -MaxLag - 1, admin, ErrDependencyKind},
	} {
		if err := AddTaskDependency(ctx, id[c.task], id[c.on], c.kind, c.lag, c.user); !errors.Is(err, c.want) {
			t.Fatalf("%s waiting on %s (%q %d): want %v, got %v", c.task, c.on, c.kind, c.lag, c.want, err)
		}
	}
	if _, _, there := how("c", "a"); there {
		t.Fatal("a refused dependency isn't made")
	}
	if err := AddTaskDependency(ctx, id["c"], id["b"], "", 0, admin); err != nil {
		t.Fatal(err)
	}
	if err := AddTaskDependency(ctx, id["a"], id["c"], "", 0, admin); !errors.Is(err, ErrDependencyLoop) {
		t.Fatalf("Design → Build → Ship → Design closes a loop: %v", err)
	}
	// Ship can start once Design has: it doesn't make Ship blocked.
	if err := AddTaskDependency(ctx, id["c"], id["a"], StartToStart, 0, admin); err != nil {
		t.Fatal(err)
	}

	// A board marks a task still waiting on open work as blocked.
	board, err := projectDomain.GetDgraphProjectTaskListForKanban(ctx, project, uids["admin"], "", dgraphStruct.BoardClosedLimit)
	if err != nil {
		t.Fatal(err)
	}
	blocked := map[string]uint32{}
	for _, tk := range board.TasksTodo {
		blocked[tk.Uuid] = tk.BlockedOpen
	}
	if blocked[id["b"]] != 1 || blocked[id["c"]] != 1 || blocked[id["a"]] != 0 {
		t.Fatalf("Build and Ship wait to start on one open task each (Ship's start to start doesn't count), Design on none: %v", blocked)
	}

	// Design slips to the 8th; Build (which overlaps it now) moves after it,
	// and a day's lag on top.
	if err := AddTaskDependency(ctx, id["b"], id["a"], FinishToStart, 1, admin); err != nil {
		t.Fatal(err)
	}
	newDue := time.Date(2026, 3, 8, 17, 0, 0, 0, time.UTC)
	before := &dgraphStruct.DgraphTask{DueDate: ptr(time.Date(2026, 3, 4, 17, 0, 0, 0, time.UTC))}
	if err := UpdateTaskDates(ctx, uuid.MustParse(id["a"]), TaskDates{Due: &newDue}, before, admin); err != nil {
		t.Fatal(err)
	}
	shifted, err := ShiftDependents(ctx, project, id["a"], time.UTC, admin)
	if err != nil || len(shifted) != 1 || shifted[0].UUID != id["b"] {
		t.Fatalf("only Build moves (Ship has no dates; the task in another project stays): %+v %v", shifted, err)
	}
	if _, start, due, lines := read("b"); !start.Equal(time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)) || !due.Equal(time.Date(2026, 3, 12, 17, 0, 0, 0, time.UTC)) || lines != 3 {
		t.Fatalf("Build starts two days after Design is due, keeps its 3 days, and says so twice in its history: %v %v %d", start, due, lines)
	}

	if err := RemoveTaskDependency(ctx, id["b"], id["a"], admin); err != nil {
		t.Fatal(err)
	}
	if by, _, _, lines := read("b"); len(by) != 0 || lines != 4 {
		t.Fatalf("Build no longer waits, and its history says so: %v %d", by, lines)
	}
	for _, on := range []string{id["a"], uuid.NewString()} {
		if err := RemoveTaskDependency(ctx, id["b"], on, admin); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, lines := read("b"); lines != 4 {
		t.Fatalf("taking off a dependency that isn't there writes no line: %d", lines)
	}
	if err := RemoveTaskDependency(ctx, id["c"], id["b"], member); !errors.Is(err, ErrNotProjectAdmin) {
		t.Fatalf("a member can't take a dependency off: %v", err)
	}
}

// Two people link Draft and Edit in opposite directions at the same moment,
// both checks passing before either writes. The project's stamp makes one
// commit abort; run again, it sees the other's link and refuses the loop.
func TestTaskDependenciesAtOnce(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live := "0001-01-01T00:00:00Z"
	d, e := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:p", "dgraph.type": "Project", "project_uuid": uuid.NewString(),
		"project_admins": []map[string]any{{"uid": "_:admin", "dgraph.type": "User", "user_uuid": uuid.NewString()}},
		"project_tasks": []map[string]any{
			{"uid": "_:d", "dgraph.type": "Task", "task_uuid": d, "task_name": "Draft", "task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:p"}},
			{"uid": "_:e", "dgraph.type": "Task", "task_uuid": e, "task_name": "Edit", "task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:p"}},
		},
	})
	var arrived sync.WaitGroup
	arrived.Add(2)
	link := func(task, on string) error {
		first := true
		_, err := taskDomain.ChangeTaskDependency(ctx, task, on, uids["admin"], false, "fs", 0, func(pair *taskDomain.DependencyPair) error {
			if first {
				// Both read the graph before either writes.
				first = false
				arrived.Done()
				arrived.Wait()
			}
			if pair.Upstream[task] {
				return ErrDependencyLoop
			}
			return nil
		})
		return err
	}
	errs := make(chan error, 2)
	go func() { errs <- link(d, e) }()
	go func() { errs <- link(e, d) }()
	var got []error
	for len(got) < 2 {
		select {
		case err := <-errs:
			got = append(got, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the two links never finished")
		}
	}
	ok, loop := 0, 0
	for _, err := range got {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDependencyLoop):
			loop++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || loop != 1 {
		t.Fatalf("one link is made and the other refused as a loop: %v", got)
	}
}

func ptr(t time.Time) *time.Time { return &t }
