package linear

import (
	"context"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// A sub-issue is left to the subtask stage, which asks for the parent's
// sub-issues; they come from the snapshot, each under its own project or its
// team's inbox. Before, none came, and every Linear sub-issue was dropped.
func TestSubIssuesComeFromTheSnapshot(t *testing.T) {
	p := New()
	job := &importModels.Job{Id: uuid.New()}
	p.snapshotCache[job.Id] = &workspaceSnapshot{Issues: []linearIssue{
		{ID: "parent", Title: "Launch", TeamID: "team", ProjectID: "proj"},
		{ID: "a", Title: "Copy", TeamID: "team", ProjectID: "proj", ParentID: "parent"},
		{ID: "b", Title: "Images", TeamID: "team", ParentID: "parent"},
		{ID: "other", Title: "Elsewhere", TeamID: "team", ParentID: "someone-else"},
	}}
	out, errs := p.IterSubtasksOfTask(context.Background(), job, nil, "parent")
	got := map[string]string{}
	for st := range out {
		if st.ParentTaskID != "parent" {
			t.Fatalf("a sub-issue names its parent: %+v", st)
		}
		got[st.SourceID] = st.ProjectSourceID
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"] != "proj" || got["b"] != inboxProjectIDPrefix+"team" {
		t.Fatalf("the parent's two sub-issues, each in its project or its team's inbox: %v", got)
	}
}
