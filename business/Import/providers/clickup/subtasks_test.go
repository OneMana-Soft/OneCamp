package clickup

import (
	"context"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// A task with a parent is left to the subtask stage, which asks for the
// parent's subtasks; they come from the snapshot. Before, none came, and every
// ClickUp subtask was dropped from an import.
func TestSubtasksComeFromTheSnapshot(t *testing.T) {
	p := New()
	job := &importModels.Job{Id: uuid.New()}
	p.snapshotCache[job.Id] = &workspaceSnapshot{Tasks: []clickupTask{
		{ID: "parent", Name: "Launch", ListID: "list"},
		{ID: "a", Name: "Copy", ListID: "list", ParentID: "parent"},
		{ID: "b", Name: "Images", ListID: "list", ParentID: "parent"},
		{ID: "other", Name: "Elsewhere", ListID: "list", ParentID: "someone-else"},
	}}
	out, errs := p.IterSubtasksOfTask(context.Background(), job, nil, "parent")
	var got []string
	for st := range out {
		if st.ParentTaskID != "parent" {
			t.Fatalf("a subtask names its parent: %+v", st)
		}
		got = append(got, st.SourceID)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("the parent's two subtasks, and no one else's: %v", got)
	}
}
