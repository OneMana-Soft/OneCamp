package Domain

import (
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The work hierarchy (jira-01 J-H) is the one parent/sub-task relation,
// three deep: epic -> task -> sub-task. These are its rules, pure.

func TestATaskGoesUnderAnEpicOrATopTaskOfItsProject(t *testing.T) {
	epic := Place{UUID: "e", Project: "p", Epic: true}
	top := Place{UUID: "t", Project: "p"}
	inEpic := Place{UUID: "c", Project: "p", Parent: "e", ParentEpic: true}
	sub := Place{UUID: "s", Project: "p", Parent: "t"}
	holding := Place{UUID: "h", Project: "p", SubTasks: 2}
	waiting := Place{UUID: "w", Project: "p", Dependent: true}
	other := Place{UUID: "o", Project: "q", Epic: true}
	for _, c := range []struct {
		name          string
		child         Place
		parent        *Place
		ok            bool
		wantInMessage string
	}{
		{"a task under an epic", top, &epic, true, ""},
		{"a task holding sub-tasks under an epic", holding, &epic, true, ""},
		{"a task with dependencies under an epic", waiting, &epic, true, ""},
		{"a sub-task under a top task", Place{UUID: "n", Project: "p"}, &top, true, ""},
		{"a sub-task under a task in an epic", Place{UUID: "n", Project: "p"}, &inEpic, true, ""},
		{"taken out to the top", sub, nil, true, ""},
		{"an epic under anything", epic, &top, false, "epic"},
		{"under a sub-task", Place{UUID: "n", Project: "p"}, &sub, false, "sub-task"},
		{"a task holding sub-tasks under a task", holding, &top, false, "sub-task"},
		{"a task with dependencies under a task", waiting, &top, false, "dependenc"},
		{"under itself", top, &top, false, "itself"},
		{"under an epic of another project", top, &other, false, "project"},
	} {
		err := MayParent(c.child, c.parent)
		if c.ok != (err == nil) {
			t.Errorf("%s: got %v", c.name, err)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrHierarchy) {
				t.Errorf("%s: %v is not a hierarchy refusal", c.name, err)
			}
			if !strings.Contains(err.Error(), c.wantInMessage) {
				t.Errorf("%s: %q doesn't say %q", c.name, err, c.wantInMessage)
			}
		}
	}
}

func TestOnlyATopTaskBecomesAnEpicAndAnEpicStaysOneWhileItsTasksNeedIt(t *testing.T) {
	if err := MayType(Place{UUID: "t", Parent: "p"}, true); !errors.Is(err, ErrHierarchy) {
		t.Errorf("a sub-task became an epic: %v", err)
	}
	if err := MayType(Place{UUID: "t", SubTasks: 3}, true); err != nil {
		t.Errorf("a top task with sub-tasks couldn't become an epic: %v", err)
	}
	if err := MayType(Place{UUID: "e", Epic: true, Deep: true}, false); !errors.Is(err, ErrHierarchy) {
		t.Errorf("an epic whose tasks hold sub-tasks stopped being one: %v", err)
	}
	if err := MayType(Place{UUID: "e", Epic: true, KidsDependent: true}, false); !errors.Is(err, ErrHierarchy) {
		t.Errorf("an epic whose tasks have dependencies stopped being one: %v", err)
	}
	if err := MayType(Place{UUID: "e", Epic: true, SubTasks: 4}, false); err != nil {
		t.Errorf("an epic of plain tasks couldn't stop being one: %v", err)
	}
}

// A top-level read counts an epic's tasks as top-level work, never a task's
// sub-tasks; with no epics it is today's clause and no block.
func TestTopLevelWorkIsWhatSitsAtTheTopOrInAnEpic(t *testing.T) {
	block, clause := topLevel(nil, true)
	if block != "" || clause != "NOT has(task_parent_task)" {
		t.Errorf("no epics: %q, %q", block, clause)
	}
	block, clause = topLevel([]string{"0b8f6f9e-2c55-4c7e-9a43-5b8c5e0a1f11", "not a uuid"}, false)
	if !strings.Contains(block, `eq(task_uuid, ["0b8f6f9e-2c55-4c7e-9a43-5b8c5e0a1f11"])`) || !strings.Contains(block, "inEpic as task_sub_tasks") {
		t.Errorf("block: %q", block)
	}
	if strings.Contains(block, "not a uuid") {
		t.Errorf("a non-uuid reached the query: %q", block)
	}
	if clause != "(NOT has(task_parent_task) OR uid(inEpic))" || strings.Contains(block, "ep as") {
		t.Errorf("a list's clause: %q, %q", block, clause)
	}
	// A total of work counts the epic's tasks, not the epic (a container).
	block, clause = topLevel([]string{"0b8f6f9e-2c55-4c7e-9a43-5b8c5e0a1f11"}, true)
	if !strings.HasPrefix(block, "ep as var(func: eq(task_uuid, [") || clause != "(NOT has(task_parent_task) OR uid(inEpic)) AND NOT uid(ep)" {
		t.Errorf("work's block and clause: %q, %q", block, clause)
	}
}

// A read across projects drops each epic from every list it read, and only
// the epics: an epic is a container in every total (the workload's too).
func TestDropEpicsLeavesTheWorkAndNotTheContainers(t *testing.T) {
	const e1, e2 = "0B8F6F9E-2C55-4C7E-9A43-5B8C5E0A1F11", "1b8f6f9e-2c55-4c7e-9a43-5b8c5e0a1f12"
	task := func(id string) *dgraphStruct.DgraphTask { return &dgraphStruct.DgraphTask{Uuid: id} }
	dated := []*dgraphStruct.DgraphTask{task("a"), task(strings.ToLower(e1)), task("b")}
	undated := []*dgraphStruct.DgraphTask{task(e2), nil, task("c")}
	none := []*dgraphStruct.DgraphTask{}
	dropEpics([]string{e1, e2}, &dated, &undated, &none)
	if len(dated) != 2 || dated[0].Uuid != "a" || dated[1].Uuid != "b" {
		t.Errorf("dated: %+v", dated)
	}
	if len(undated) != 1 || undated[0].Uuid != "c" || len(none) != 0 {
		t.Errorf("undated: %+v, none: %+v", undated, none)
	}
	kept := []*dgraphStruct.DgraphTask{task("a")}
	dropEpics(nil, &kept)
	if len(kept) != 1 {
		t.Errorf("no epics drops nothing: %+v", kept)
	}
}
