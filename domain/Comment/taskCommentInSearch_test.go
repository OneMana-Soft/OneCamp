package domain

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A task comment's search entry names its task and its task's project, which
// is how search shows it to the project's people and keeps a restricted task's
// comments from the rest, and holds its text as plain text. An import's entry
// had neither the project nor plain text, so no one found an imported comment.
func TestATaskCommentsSearchEntryNamesItsProjectAndTask(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	by := &dgraphStruct.DgraphUser{Uuid: "u-rae", UserFullName: "Rae Kim"}
	task := &dgraphStruct.DgraphTask{Uuid: "t-hr4", Project: &dgraphStruct.DgraphProject{Uuid: "p-hr", Name: "HR"}}

	e := TaskCommentInSearch(task, "c-41", by, "<p>Quokka <b>bonus</b> pool</p><p>for the leads</p>", at)
	if e.Uuid != "c-41" || e.CommentTaskUuid != "t-hr4" || e.CommentProjectUuid != "p-hr" || e.CommentProjectName != "HR" {
		t.Errorf("the entry's comment, task and project: %q %q %q %q", e.Uuid, e.CommentTaskUuid, e.CommentProjectUuid, e.CommentProjectName)
	}
	if e.CommentBody != "Quokka bonus pool for the leads" {
		t.Errorf("the entry's text: %q", e.CommentBody)
	}
	if e.CommentByUserUuid != "u-rae" || e.CommentByUserFullName != "Rae Kim" || e.CommentCreatedAt != at.Unix() {
		t.Errorf("the entry's author and time: %q %q %d", e.CommentByUserUuid, e.CommentByUserFullName, e.CommentCreatedAt)
	}

	// A task in no project: its entry names its task and no project.
	if e := TaskCommentInSearch(&dgraphStruct.DgraphTask{Uuid: "t-1"}, "c-1", by, "hi", at); e.CommentTaskUuid != "t-1" || e.CommentProjectUuid != "" || e.CommentProjectName != "" {
		t.Errorf("a task in no project: task %q, project %q %q", e.CommentTaskUuid, e.CommentProjectUuid, e.CommentProjectName)
	}
}
