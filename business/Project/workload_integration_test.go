//go:build integration

package business

// The workload against a real Dgraph with the production schema.
// Run: go test -tags=integration ./business/Project/ -run TestWorkload -v

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestWorkload(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", "2026-09-01T00:00:00Z"
	id := map[string]string{}
	for _, k := range []string{"me", "alice", "bob", "bot", "left", "moved"} {
		id[k] = uuid.NewString()
	}
	person := func(key, name string, more map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id[key], "user_name": name}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	n := 0
	task := func(name string, more map[string]any) map[string]any {
		n++
		m := map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": name, "task_status": "todo", "task_deleted_at": live}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	on := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": id["me"], "user_name": "Me",
		"user_projects": []map[string]any{
			{
				"uid": "_:p", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "Launch",
				"project_admins": []map[string]any{on("me")},
				"project_members": []map[string]any{
					person("alice", "Alice", map[string]any{"user_weekly_capacity": 3}),
					person("bot", "Release Captain", map[string]any{"is_bot": true}),
					person("left", "Left", map[string]any{"user_deleted_at": gone}),
				},
				"project_tasks": []map[string]any{
					task("Spec", map[string]any{"task_assignee": on("alice"), "task_start_date": "2026-10-05T09:00:00Z", "task_due_date": "2026-10-09T17:00:00Z"}),
					task("Review", map[string]any{"task_assignee": on("alice"), "task_due_date": "2026-10-14T17:00:00Z"}),
					task("Nobody's", map[string]any{"task_due_date": "2026-10-15T17:00:00Z"}),
					task("Agent's", map[string]any{"task_assignee": on("bot"), "task_due_date": "2026-10-15T17:00:00Z"}),
					task("Orphaned", map[string]any{"task_assignee": on("left"), "task_due_date": "2026-10-16T17:00:00Z"}),
					// Moved was taken off the project and kept this task.
					task("Handover", map[string]any{"task_assignee": person("moved", "Moved", nil), "task_due_date": "2026-10-16T17:00:00Z"}),
					task("Shipped", map[string]any{"task_assignee": on("alice"), "task_status": "done", "task_due_date": "2026-10-06T17:00:00Z"}),
					task("Removed", map[string]any{"task_assignee": on("alice"), "task_deleted_at": gone, "task_due_date": "2026-10-06T17:00:00Z"}),
					task("Someday", map[string]any{"task_assignee": on("alice")}),
					task("Someday, nobody's", map[string]any{"task_start_date": "0001-01-01T00:00:00Z", "task_due_date": "0001-01-01T00:00:00Z"}),
					task("Next year", map[string]any{"task_assignee": on("alice"), "task_start_date": "2027-02-01T09:00:00Z"}),
				},
			},
			{
				"uid": "_:q", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "Site",
				"project_members": []map[string]any{on("me"), person("bob", "Bob", nil)},
				"project_tasks": []map[string]any{
					task("Copy", map[string]any{"task_assignee": on("bob"), "task_start_date": "2026-10-12T09:00:00Z", "task_due_date": "2026-10-23T17:00:00Z"}),
				},
			},
			{
				"uid": "_:r", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "Archived", "project_deleted_at": gone,
				"project_members": []map[string]any{on("me")},
				"project_tasks":   []map[string]any{task("Old", map[string]any{"task_assignee": on("me"), "task_due_date": "2026-10-06T17:00:00Z"})},
			},
		},
	})
	// A subtask counts as the overview counts it.
	dg.Mutate(t, map[string]any{"uid": uids["q"], "project_tasks": []map[string]any{
		task("Proofread", map[string]any{"task_assignee": map[string]any{"uid": uids["bob"]}, "task_due_date": "2026-10-20T17:00:00Z"}),
	}})

	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	w, err := GetWorkload(ctx, uids["me"], id["me"], false, until)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range w.People {
		names = append(names, p.Name)
	}
	if len(names) != 4 || names[0] != "Alice" || names[1] != "Bob" || names[2] != "Me" || names[3] != "Moved" {
		t.Fatalf("the people of the live projects and those with tasks in them, no bot, no deleted account: %v", names)
	}
	alice, bob, me, moved := w.People[0], w.People[1], w.People[2], w.People[3]
	if len(moved.Projects) != 0 {
		t.Fatalf("someone with a task in a project they aren't in isn't counted as in it: %v", moved.Projects)
	}
	if alice.Capacity != 3 || !alice.CapacitySet || alice.CanEdit {
		t.Fatalf("Alice takes on 3 a week and isn't the reader's to change: %+v", alice)
	}
	if me.Capacity != DefaultWeeklyCapacity || me.CapacitySet || !me.CanEdit {
		t.Fatalf("the reader has the default and may change their own: %+v", me)
	}
	launch, site := w.Tasks[0].ProjectUUID, ""
	for _, tk := range w.Tasks {
		if tk.Name == "Spec" {
			launch = tk.ProjectUUID
		}
		if tk.Name == "Copy" {
			site = tk.ProjectUUID
		}
	}
	if len(alice.Projects) != 1 || alice.Projects[0] != launch || len(bob.Projects) != 1 || bob.Projects[0] != site || len(me.Projects) != 2 {
		t.Fatalf("each person's live projects, so a view of some projects keeps to their people: %v %v %v", alice.Projects, bob.Projects, me.Projects)
	}
	undated := map[string]int{}
	for _, u := range w.Undated {
		undated[u.ProjectUUID+"/"+u.UserUUID] += u.Count
	}
	if len(undated) != 2 || undated[launch+"/"+id["alice"]] != 1 || undated[launch+"/"] != 1 {
		t.Fatalf("one task of Launch without dates is Alice's, one nobody's: %v", undated)
	}
	got := map[string]WorkloadTask{}
	var tasks []string
	for _, tk := range w.Tasks {
		got[tk.Name] = tk
		tasks = append(tasks, tk.Name)
	}
	sort.Strings(tasks)
	want := []string{"Copy", "Handover", "Nobody's", "Orphaned", "Proofread", "Review", "Spec"}
	if len(tasks) != len(want) {
		t.Fatalf("open, live, dated tasks of live projects that start in time, none an agent's: %v", tasks)
	}
	for i := range want {
		if tasks[i] != want[i] {
			t.Fatalf("want %v, got %v", want, tasks)
		}
	}
	if got["Spec"].AssigneeUUID != id["alice"] || !got["Spec"].CanEdit || got["Spec"].Start == nil || got["Spec"].ProjectName != "Launch" {
		t.Fatalf("Spec is Alice's, in Launch, which the reader runs: %+v", got["Spec"])
	}
	if got["Orphaned"].AssigneeUUID != "" || got["Nobody's"].AssigneeUUID != "" {
		t.Fatal("a deleted account's task counts as nobody's")
	}
	if got["Handover"].AssigneeUUID != id["moved"] {
		t.Fatal("a task stays its assignee's after they leave the project")
	}
	if got["Copy"].CanEdit || got["Review"].Start != nil {
		t.Fatalf("the reader is only a member of Site; Review has only a due date: %+v %+v", got["Copy"], got["Review"])
	}

	// Capacity: their own, or anyone's for a workspace admin.
	if err := SetWeeklyCapacity(ctx, id["me"], false, id["alice"], 4); !errors.Is(err, ErrCapacityNotYours) {
		t.Fatalf("a member can't change someone else's: %v", err)
	}
	for _, bad := range []int{-1, MaxWeeklyCapacity + 1} {
		if err := SetWeeklyCapacity(ctx, id["me"], false, id["me"], bad); !errors.Is(err, ErrCapacityRange) {
			t.Fatalf("%d is out of range: %v", bad, err)
		}
	}
	if err := SetWeeklyCapacity(ctx, id["me"], true, uuid.NewString(), 4); !errors.Is(err, ErrPersonNotFound) {
		t.Fatalf("nobody by that id: %v", err)
	}
	if err := SetWeeklyCapacity(ctx, id["me"], false, id["me"], 8); err != nil {
		t.Fatal(err)
	}
	if err := SetWeeklyCapacity(ctx, id["me"], true, id["alice"], 0); err != nil {
		t.Fatal(err)
	}
	w, err = GetWorkload(ctx, uids["me"], id["me"], true, until)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range w.People {
		switch p.Name {
		case "Me":
			if p.Capacity != 8 || !p.CapacitySet {
				t.Fatalf("the reader set 8: %+v", p)
			}
		case "Alice":
			if p.Capacity != DefaultWeeklyCapacity || p.CapacitySet || !p.CanEdit {
				t.Fatalf("an admin put Alice back to the default, and may change it: %+v", p)
			}
		}
	}
}

// A project with 3,000 old tasks and one task that starts today, with no due
// date: the cap must not cut today's for last year's.
func TestWorkloadStartOnlyTasksSurviveTheCap(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	me := uuid.NewString()
	var tasks []map[string]any
	for i := 0; i < 3000; i++ {
		tasks = append(tasks, map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "old", "task_status": "todo", "task_deleted_at": "0001-01-01T00:00:00Z", "task_due_date": "2025-01-01T17:00:00Z"})
	}
	tasks = append(tasks, map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "THIS WEEK start-only", "task_status": "todo", "task_deleted_at": "0001-01-01T00:00:00Z", "task_start_date": time.Now().UTC().Format(time.RFC3339)})
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": me, "user_name": "Me",
		"user_projects": []map[string]any{{"uid": "_:p", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "P",
			"project_admins": []map[string]any{{"uid": "_:me"}}, "project_tasks": tasks}},
	})
	w, err := GetWorkload(ctx, uids["me"], me, false, time.Now().AddDate(0, 0, 91))
	if err != nil {
		t.Fatal(err)
	}
	if w.Truncated {
		t.Fatal("3,000 tasks with a due date fit the cap: nothing was cut")
	}
	for _, tk := range w.Tasks {
		if tk.Name == "THIS WEEK start-only" {
			return
		}
	}
	t.Fatalf("a start-only task this week was cut for tasks overdue a year: %d tasks kept", len(w.Tasks))
}

// Tasks without dates count as dated ones do: a deleted account's is
// nobody's, and someone taken off the project keeps theirs.
func TestWorkloadUndatedOfFormerPeople(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", "2026-09-01T00:00:00Z"
	me, left, ex := uuid.NewString(), uuid.NewString(), uuid.NewString()
	task := func(name string, more map[string]any) map[string]any {
		m := map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": name, "task_status": "todo", "task_deleted_at": live}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": me, "user_name": "Me",
		"user_projects": []map[string]any{{
			"uid": "_:p", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "P",
			"project_admins":  []map[string]any{{"uid": "_:me"}},
			"project_members": []map[string]any{{"uid": "_:left", "dgraph.type": "User", "user_uuid": left, "user_name": "Left", "user_deleted_at": gone}},
			"project_tasks": []map[string]any{
				task("Deleted user's, undated", map[string]any{"task_assignee": map[string]any{"uid": "_:left"}}),
				task("Ex-member's, undated", map[string]any{"task_assignee": map[string]any{"uid": "_:ex", "dgraph.type": "User", "user_uuid": ex, "user_name": "Ex"}}),
			},
		}},
	})
	w, err := GetWorkload(ctx, uids["me"], me, false, time.Now().AddDate(0, 0, 91))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, u := range w.Undated {
		got[u.UserUUID] += u.Count
	}
	if len(got) != 2 || got[""] != 1 || got[ex] != 1 {
		t.Fatalf("a deleted account's task without dates is nobody's, an ex-member's stays theirs: %v", got)
	}
}
