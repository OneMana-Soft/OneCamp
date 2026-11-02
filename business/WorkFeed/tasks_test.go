package business

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	taskA    = "6f1c2b9e-0d4a-4c7e-9a51-1b2c3d4e5f60"
	taskB    = "7a2d3c0f-1e5b-4d8f-8b62-2c3d4e5f6071"
	projectP = "8b3e4d1a-2f6c-4e9a-9c73-3d4e5f607182"
	ada      = "9c4f5e2b-3a7d-4fab-8d84-4e5f60718293"
)

// A change's diff is the named fields as the app's task answer names them:
// someone it names as lists show people, never with their email or graph id;
// a parent task by its id and name; a field the task leaves unset as null, so
// a cleared value reads as cleared.
func TestTaskDiffCarriesTheAnswersOwnFieldsAndNoEmail(t *testing.T) {
	key := "pics/ada.png"
	task := &dgraphStruct.DgraphTask{Uuid: taskA, Name: "Ship it", Priority: "high",
		Assignee: &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: ada, UserName: "ada", UserFullName: "Ada L",
			EmailID: "ada@example.test", ProfileKey: &key},
		ParentTask: &dgraphStruct.DgraphTask{Uuid: taskB, Name: "Launch", Priority: "low"}}
	var got map[string]any
	if err := json.Unmarshal(TaskDiff(task, "task_name", "task_priority", "task_assignee", "task_parent_task", "task_status"), &got); err != nil {
		t.Fatal(err)
	}
	want := "map[task_assignee:map[user_full_name:Ada L user_name:ada user_profile_object_key:pics/ada.png user_uuid:" + ada +
		"] task_name:Ship it task_parent_task:map[task_name:Launch task_uuid:" + taskB + "] task_priority:high task_status:<nil>]"
	if fmt.Sprint(got) != want {
		t.Errorf("diff %v\nwant %s", got, want)
	}
	for _, a := range []*dgraphStruct.DgraphUser{nil, {UserName: "ghost"}} {
		if got := string(TaskDiff(&dgraphStruct.DgraphTask{Uuid: taskA, Assignee: a}, "task_assignee")); got != `{"task_assignee":null}` {
			t.Errorf("assignee %+v: %s, want it cleared", a, got)
		}
	}
	if TaskDiff(task) != nil || TaskDiff(nil, "task_name") != nil {
		t.Error("no fields, or no task, is no diff")
	}
}

// RecordTasks logs each change that names a task and a project, with who
// made it: a change naming neither is nobody's to read and isn't logged; a
// maker that isn't a person is no one; a diff given (a field's value) is
// carried as it is; a restricted task's change is marked so.
func TestRecordTasksLogsWhatItCanName(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	saved := restrictedTasks
	t.Cleanup(func() { restrictedTasks = saved })
	restrictedTasks = func(context.Context, []string) map[string]bool { return map[string]bool{taskB: true} }
	task := func(id string) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: id, Name: "named " + id[:4]}
	}
	ctx := context.Background()
	got := RecordTasks(ctx, ada,
		TaskChange{Kind: KindUpdated, Project: projectP, Task: task(taskA), Fields: []string{"task_name"}},
		TaskChange{Kind: KindUpdated, Project: projectP},
		TaskChange{Kind: KindUpdated, Project: projectP, Task: task("not-a-task")},
		TaskChange{Kind: KindUpdated, Project: "nowhere", Task: task(taskA)},
		TaskChange{Kind: KindUpdated, Project: projectP, Task: task(taskB), Diff: json.RawMessage(`{"field_id":"f1","value":3}`)},
	)
	if len(got) != 2 || len(l.rows) != 2 {
		t.Fatalf("logged %d (answered %d), want the two that name a task and a project", len(l.rows), len(got))
	}
	a, b := l.rows[0], l.rows[1]
	if a.ResourceUUID != taskA || a.ProjectUUID != projectP || a.Kind != KindUpdated || a.Actor != ada || a.Source != "app" ||
		a.Restricted || string(a.Diff) != `{"task_name":"named 6f1c"}` {
		t.Errorf("first: %+v", a)
	}
	if b.ResourceUUID != taskB || !b.Restricted || string(b.Diff) != `{"field_id":"f1","value":3}` {
		t.Errorf("second: %+v, want restricted with the field's own diff", b)
	}
	RecordTasks(ctx, "0x12", TaskChange{Kind: KindDeleted, Project: projectP, Task: task(taskA)})
	if last := l.rows[len(l.rows)-1]; last.Actor != "" || last.Kind != KindDeleted {
		t.Errorf("by a graph id: %+v, want no one", last)
	}
	if RecordTasks(ctx, ada) != nil || RecordTasks(ctx, ada, TaskChange{Project: projectP}) != nil {
		t.Error("nothing to log answers nothing")
	}
}

// Where a change came from: an agent's run, an API token (the public API or
// MCP), or the app.
func TestRecordTasksSaysWhereAChangeCameFrom(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	saved := restrictedTasks
	t.Cleanup(func() { restrictedTasks = saved })
	restrictedTasks = func(context.Context, []string) map[string]bool { return nil }
	bg := context.Background()
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{
		{bg, "app"},
		{helpers.WithActsThroughToken(bg), "api"},
		{helpers.WithAgentWrite(bg), "agent"},
		{helpers.WithAgentWrite(helpers.WithActsThroughToken(bg)), "agent"},
	} {
		RecordTasks(c.ctx, ada, TaskChange{Kind: KindUpdated, Project: projectP, Task: &dgraphStruct.DgraphTask{Uuid: taskA}, Fields: []string{"task_name"}})
		if got := l.rows[len(l.rows)-1].Source; got != c.want {
			t.Errorf("source %q, want %q", got, c.want)
		}
	}
}

// More than MaxToldOneByOne entries of one project in one write (a
// renumbered column, a pasted list) are told once, as a Reread; another
// project's few are still told one by one, and exactly MaxToldOneByOne is.
func TestABurstIsToldAsOneReread(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	saved := publishReread
	t.Cleanup(func() { publishReread = saved })
	var reread []string
	publishReread = func(_ context.Context, project string) { reread = append(reread, project) }
	var burst []Change
	for i := 0; i <= MaxToldOneByOne; i++ {
		burst = append(burst, change(fmt.Sprintf("t%d", i), KindUpdated))
	}
	other := change("o1", KindUpdated)
	other.ProjectUUID = "p2"
	Record(context.Background(), append(burst, other)...)
	if len(reread) != 1 || reread[0] != "p1" || len(l.published) != 1 || l.published[0].ProjectUUID != "p2" {
		t.Fatalf("reread %v, told %d one by one: want p1 once and p2's one", reread, len(l.published))
	}
	l.published, reread = nil, nil
	Record(context.Background(), burst[:MaxToldOneByOne]...)
	if len(reread) != 0 || len(l.published) != MaxToldOneByOne {
		t.Errorf("exactly %d: reread %v, told %d", MaxToldOneByOne, reread, len(l.published))
	}
}
