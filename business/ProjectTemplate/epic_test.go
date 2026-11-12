package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A template keeps the whole hierarchy (jira-01 J-H): an epic with its tasks
// and their sub-tasks, as deep as the hierarchy goes and no deeper; only an
// epic holds tasks.
func TestATemplateKeepsAnEpicWithItsTasksAndTheirSubTasks(t *testing.T) {
	sub := &dgraphStruct.DgraphTask{Uuid: "s", Name: "Sub", Status: "todo"}
	child := &dgraphStruct.DgraphTask{Uuid: "c", Name: "Child", Status: "todo", SubTasks: []*dgraphStruct.DgraphTask{sub}}
	epic := &dgraphStruct.DgraphTask{Uuid: "e", Name: "Epic", Status: "todo", SubTasks: []*dgraphStruct.DgraphTask{child}}
	plain := &dgraphStruct.DgraphTask{Uuid: "p", Name: "Plain", Status: "todo", SubTasks: []*dgraphStruct.DgraphTask{{Uuid: "q", Name: "Step", Status: "todo"}}}
	got := FromProject("x", "", nil, []*dgraphStruct.DgraphTask{epic, plain}, map[string]bool{"e": true}, time.UTC)
	if len(got.Tasks) != 2 || !got.Tasks[0].Epic || len(got.Tasks[0].Subtasks) != 0 || len(got.Tasks[0].Tasks) != 1 {
		t.Fatalf("the epic isn't kept with its tasks: %+v", got.Tasks)
	}
	if c := got.Tasks[0].Tasks[0]; c.Name != "Child" || c.Epic || len(c.Subtasks) != 1 || c.Subtasks[0].Name != "Sub" {
		t.Errorf("the epic's task isn't kept with its sub-task: %+v", c)
	}
	if p := got.Tasks[1]; p.Epic || len(p.Tasks) != 0 || len(p.Subtasks) != 1 {
		t.Errorf("a plain task isn't kept as it was: %+v", p)
	}
	if n := got.Size(); n != 5 {
		t.Errorf("size %d, want 5: an epic's tasks and their sub-tasks count", n)
	}
	if _, err := Check(got); err != nil {
		t.Errorf("the template doesn't check: %v", err)
	}

	for name, bad := range map[string]Template{
		"a task that isn't an epic holds tasks": {Name: "x", Tasks: []Task{{Name: "T", Tasks: []Task{{Name: "U"}}}}},
		"an epic in an epic":                    {Name: "x", Tasks: []Task{{Name: "E", Epic: true, Tasks: []Task{{Name: "F", Epic: true}}}}},
		"an epic's task holds tasks":            {Name: "x", Tasks: []Task{{Name: "E", Epic: true, Tasks: []Task{{Name: "T", Tasks: []Task{{Name: "U"}}}}}}},
		"an epic's task has no name":            {Name: "x", Tasks: []Task{{Name: "E", Epic: true, Tasks: []Task{{Name: " "}}}}},
	} {
		if _, err := Check(bad); err == nil {
			t.Errorf("%s: checked", name)
		}
	}

	batch := batchOf(got, "proj", nil, Start{})
	if len(batch) != 5 {
		t.Fatalf("a batch of %d, want 5", len(batch))
	}
	for i, want := range []struct {
		name   string
		parent int
		epic   bool
	}{{"Epic", -1, true}, {"Child", 0, false}, {"Sub", 1, false}, {"Plain", -1, false}, {"Step", 3, false}} {
		if b := batch[i]; b.Input.TaskName != want.name || b.Parent != want.parent || b.Epic != want.epic {
			t.Errorf("batch %d: %q under %d epic %v, want %+v", i, b.Input.TaskName, b.Parent, b.Epic, want)
		}
	}
}
