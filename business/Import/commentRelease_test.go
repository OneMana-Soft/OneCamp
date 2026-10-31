package business

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// The comments the search repair held back are let in once their import's
// re-check has settled: each comment that import brought, through the
// repair's own page, once. A comment the re-check hid stays out, and an
// import that hasn't settled keeps its comments held back.
func TestTheCommentsOfASettledImportAreLetIntoSearch(t *testing.T) {
	f := newFakeCommentSearch(t)
	fix := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	before, hidden := fix.Add(-24*time.Hour), fix.Add(time.Hour)
	f.fixed[JiraRestrictionsFixed] = fix
	settledJob, heldJob := uuid.New(), uuid.New()
	f.settled[settledJob] = true
	by := &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: "ada"}
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString(), Name: "HR"}}
	live, hid, held := "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"
	of := map[string]uuid.UUID{live: settledJob, hid: settledJob, held: heldJob}
	for id, job := range of {
		c := &dgraphStruct.DgraphComment{Text: "<p>The quokka plan</p>", Task: task, CommentBy: by, CreatedAt: &before}
		if id == hid {
			c.DeletedAt = &hidden
		}
		f.comment(id, c)
		f.imports[id] = []importModels.CommentImport{{Comment: uuid.MustParse(id), Import: job, Provider: importModels.ProviderJira, At: &before}}
	}
	var released []uuid.UUID
	oldPending, oldIDs, oldMark := unreleasedImports, importCommentIDs, markReleased
	t.Cleanup(func() { unreleasedImports, importCommentIDs, markReleased = oldPending, oldIDs, oldMark })
	unreleasedImports = func(context.Context) ([]uuid.UUID, error) {
		var out []uuid.UUID
		for job := range f.settled {
			if !slices.Contains(released, job) {
				out = append(out, job)
			}
		}
		return out, nil
	}
	importCommentIDs = func(_ context.Context, job, after uuid.UUID, limit int) ([]uuid.UUID, error) {
		var out []uuid.UUID
		for _, id := range []string{live, hid, held} {
			if c := uuid.MustParse(id); of[id] == job && bytes.Compare(c[:], after[:]) > 0 && len(out) < limit {
				out = append(out, c)
			}
		}
		return out, nil
	}
	markReleased = func(_ context.Context, job uuid.UUID) error {
		released = append(released, job)
		return nil
	}

	if err := releaseSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.rewritten[live] == nil {
		t.Errorf("the settled import's comment wasn't let into search")
	}
	if f.rewritten[hid] != nil {
		t.Errorf("a comment the re-check hid was let into search")
	}
	if f.rewritten[held] != nil {
		t.Errorf("a comment of an import that hasn't settled was let into search")
	}
	if !slices.Equal(released, []uuid.UUID{settledJob}) {
		t.Errorf("recorded as let in: %v; want only the settled import %s", released, settledJob)
	}
	clear(f.rewritten)
	if err := releaseSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.rewritten) != 0 {
		t.Errorf("a second pass let in %d comments again", len(f.rewritten))
	}
}
