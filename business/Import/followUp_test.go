package business

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// I1 (importer fidelity): Jira, Asana and Todoist imported no comments. A
// task's comments chunk was queued only on a count the provider gave, and
// those three gave none. A provider that can't count them says so
// (CommentsUnknown) and the comments stage asks; one that knows there are
// none asks nothing.

func kinds(cs []*importModels.Chunk) map[string]int {
	out := map[string]int{}
	for _, c := range cs {
		out[string(c.ChunkType)]++
	}
	return out
}

func TestATaskWhoseCommentsArentCountedIsAskedForThem(t *testing.T) {
	job := &importModels.Job{Id: uuid.New()}
	unknown := followUpChunks(job, &importProvider.SourceTask{SourceID: "T-1", CommentCount: importProvider.CommentsUnknown})
	if kinds(unknown)[string(importModels.ChunkTaskComments)] != 1 {
		t.Fatalf("an uncounted task's comments aren't asked for: %v", kinds(unknown))
	}
	if unknown[0].ItemsTotal != nil {
		t.Fatalf("an uncounted task's comments have a total: %d", *unknown[0].ItemsTotal)
	}
	if none := followUpChunks(job, &importProvider.SourceTask{SourceID: "T-2"}); len(none) != 0 {
		t.Fatalf("a task with no comments asks for some: %v", kinds(none))
	}
	if two := followUpChunks(job, &importProvider.SourceTask{SourceID: "T-3", CommentCount: 2}); *two[0].ItemsTotal != 2 {
		t.Fatalf("a counted task's total: %v", two[0].ItemsTotal)
	}
}

func TestFilesListedWithATasksCommentsAreQueuedAsTheTasks(t *testing.T) {
	job := &importModels.Job{Id: uuid.New()}
	got := fileChunks(job, "T-1", "task", []importProvider.SourceAttachment{
		{SourceID: "a1", Name: "spec.pdf", URL: "https://x/a1", Parent: importProvider.SourceRef{Kind: "task", SourceID: "T-1"}},
		{SourceID: "a2", Name: "plan.png", URL: "https://x/a2", Parent: importProvider.SourceRef{Kind: "task", SourceID: "T-1"}},
	})
	if len(got) != 2 || kinds(got)[string(importModels.ChunkAttachment)] != 2 || *got[0].ParentSourceId != "T-1" {
		t.Fatalf("files: %v", kinds(got))
	}
}
