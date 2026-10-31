package business

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// fakeCommentSearch stands in for search, the graph and the imports' records
// under the repair: the entries that name no project, in id order, the
// comments the graph holds by id, the imports that brought each in, and when
// each fix was first run (by key). It records what was rewritten, and which
// comments were marked deleted with their archived projects, and when.
type fakeCommentSearch struct {
	ids       []string
	comments  map[string]*dgraphStruct.DgraphComment
	imports   map[string][]importModels.CommentImport
	fixed     map[string]time.Time
	settled   map[uuid.UUID]bool
	rewritten map[string]*openSearchStruct.OpenSearchComment
	archived  map[string]int64
	reads     int
	down      error
}

func newFakeCommentSearch(t *testing.T) *fakeCommentSearch {
	t.Helper()
	f := &fakeCommentSearch{comments: map[string]*dgraphStruct.DgraphComment{}, imports: map[string][]importModels.CommentImport{},
		fixed: map[string]time.Time{}, settled: map[uuid.UUID]bool{}, rewritten: map[string]*openSearchStruct.OpenSearchComment{}, archived: map[string]int64{}}
	oldPage, oldComments, oldRewrite, oldImports, oldFixed, oldSettled := entriesWithoutProject, commentsForSearch, rewriteEntries, commentImports, fixMoment, settledImports
	t.Cleanup(func() {
		entriesWithoutProject, commentsForSearch, rewriteEntries, commentImports, fixMoment, settledImports = oldPage, oldComments, oldRewrite, oldImports, oldFixed, oldSettled
	})
	settledImports = func(context.Context) (map[uuid.UUID]bool, error) { return f.settled, nil }
	commentImports = func(_ context.Context, ids []uuid.UUID) ([]importModels.CommentImport, error) {
		var out []importModels.CommentImport
		for _, id := range ids {
			out = append(out, f.imports[id.String()]...)
		}
		return out, nil
	}
	fixMoment = func(key string) (time.Time, bool, error) {
		at, ok := f.fixed[key]
		return at, ok, nil
	}
	entriesWithoutProject = func(_ context.Context, after string, size int) ([]string, string, error) {
		from := 0
		for from < len(f.ids) && after != "" && f.ids[from] <= after {
			from++
		}
		page := f.ids[from:min(from+size, len(f.ids))]
		if len(page) < size {
			return page, "", nil
		}
		return page, page[len(page)-1], nil
	}
	commentsForSearch = func(_ context.Context, ids []uuid.UUID) ([]*dgraphStruct.DgraphComment, error) {
		f.reads++
		if f.down != nil {
			return nil, f.down
		}
		var out []*dgraphStruct.DgraphComment
		for _, id := range ids {
			if c, ok := f.comments[id.String()]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	}
	rewriteEntries = func(_ context.Context, entries map[string]*openSearchStruct.OpenSearchComment, archived map[string]int64) (int, error) {
		for id, e := range entries {
			f.rewritten[id] = e
		}
		for id, at := range archived {
			if entries[id] == nil {
				t.Errorf("comment %s marked archived without its entry in the same write", id)
			}
			f.archived[id] = at
		}
		return len(entries), nil
	}
	return f
}

// comment adds an entry that names no project, and its comment as the graph
// holds it (none for a comment the graph doesn't hold), brought in by an
// Asana import, whose comments are never held back.
func (f *fakeCommentSearch) comment(id string, c *dgraphStruct.DgraphComment) {
	f.ids = append(f.ids, id)
	if c != nil {
		c.Uuid = id
		f.comments[id] = c
	}
	f.importedBy(id, importModels.ProviderAsana, nil)
}

// importedBy says which imports brought comment id in: one of provider at at
// for each pair, none when called with no provider.
func (f *fakeCommentSearch) importedBy(id, provider string, at *time.Time) {
	f.imports[id] = nil
	if provider != "" {
		f.imports[id] = []importModels.CommentImport{{Comment: uuid.MustParse(id), Provider: provider, At: at}}
	}
}

func TestTheRepairRewritesWhatItCanAndLeavesTheRest(t *testing.T) {
	f := newFakeCommentSearch(t)
	written := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	gone := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	by := &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: "ada"}
	task := func(project string, archived *time.Time) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: project, Name: "Harbor", DeletedAt: archived}}
	}
	live, old := uuid.NewString(), uuid.NewString()
	ids := make([]string, 6)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	f.comment(ids[0], &dgraphStruct.DgraphComment{Text: "<p>The <strong>quokka</strong> plan</p>", Task: task(live, nil), CommentBy: by, CreatedAt: &written})
	f.comment(ids[1], &dgraphStruct.DgraphComment{Text: "<p>On the old pier</p>", Task: task(old, &gone), CommentBy: by, CreatedAt: &written})
	f.comment(ids[2], &dgraphStruct.DgraphComment{Text: "<p>Withdrawn</p>", Task: task(live, nil), CommentBy: by, CreatedAt: &written, DeletedAt: &gone})
	f.comment(ids[3], &dgraphStruct.DgraphComment{Text: "<p>No task</p>", CommentBy: by, CreatedAt: &written})
	f.comment(ids[4], nil)
	f.comment(ids[5], &dgraphStruct.DgraphComment{Text: "<p>No writer</p>", Task: task(live, nil), CreatedAt: &written})

	rep, err := RepairTaskCommentSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := TaskCommentSearchRepair{Entries: 6, Rewritten: 2, Archived: 1, Deleted: 1, Orphaned: 3}
	if rep != want {
		t.Errorf("the repair did %+v; want %+v", rep, want)
	}
	e := f.rewritten[ids[0]]
	if e == nil || e.CommentProjectUuid != live || e.CommentProjectName != "Harbor" || e.CommentBody != "The quokka plan" ||
		e.CommentTaskUuid != f.comments[ids[0]].Task.Uuid || e.CommentByUserUuid != by.Uuid || e.CommentCreatedAt != written.Unix() {
		t.Errorf("the live comment's entry: %+v; want a person's comment's entry, its project's and in plain text", e)
	}
	if len(f.rewritten) != 2 || f.rewritten[ids[1]] == nil {
		t.Errorf("rewritten %d entries; want the live comment's and the archived project's", len(f.rewritten))
	}
	if len(f.archived) != 1 || f.archived[ids[1]] != gone.Unix() {
		t.Errorf("comments marked with their archived projects: %v; want the old pier comment, at its project's archiving", f.archived)
	}
}

func TestTheRepairPagesThroughEveryEntryOnce(t *testing.T) {
	f := newFakeCommentSearch(t)
	written := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	by := &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: "ada"}
	project := uuid.NewString()
	n := taskCommentSearchRepairPage*2 + 7
	for i := 0; i < n; i++ {
		f.comment(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), &dgraphStruct.DgraphComment{Text: "<p>A note</p>",
			Task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: project, Name: "Harbor"}}, CommentBy: by, CreatedAt: &written})
	}
	rep, err := RepairTaskCommentSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != n || rep.Rewritten != n || len(f.rewritten) != n || f.reads != 3 {
		t.Errorf("%d entries: the repair did %+v in %d reads, rewrote %d; want each once, in 3 pages", n, rep, f.reads, len(f.rewritten))
	}
}

func TestTheRepairStopsWhenTheGraphCantBeRead(t *testing.T) {
	f := newFakeCommentSearch(t)
	f.down = errors.New("the graph didn't answer")
	for i := 0; i < taskCommentSearchRepairPage+1; i++ {
		f.comment(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), nil)
	}
	if _, err := RepairTaskCommentSearch(context.Background()); !errors.Is(err, f.down) {
		t.Errorf("the repair answered %v; want the graph's error", err)
	}
	if f.reads != 1 || len(f.rewritten) != 0 {
		t.Errorf("after the graph failed: %d reads, %d rewritten; want it to stop at once", f.reads, len(f.rewritten))
	}
}

func TestOnlyCommentsKnownSafeLeaveTheHoldBack(t *testing.T) {
	fix := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	before, after := fix.Add(-time.Hour), fix.Add(time.Hour)
	fixed := map[string]time.Time{
		importModels.ProviderJira: fix, importModels.ProviderTodoist: fix, importModels.ProviderNotion: fix, importModels.ProviderBasecamp: fix,
	}
	of := func(provider string, at *time.Time) importModels.CommentImport {
		return importModels.CommentImport{Comment: uuid.New(), Import: uuid.New(), Provider: provider, At: at}
	}
	// An import whose re-check against its source has settled.
	settledJira, settledUndated := of(importModels.ProviderJira, &before), of(importModels.ProviderJira, nil)
	settled := map[uuid.UUID]bool{settledJira.Import: true, settledUndated.Import: true}
	for _, c := range []struct {
		name string
		in   []importModels.CommentImport
		held bool
	}{
		{"a Jira import before the fix, re-checked since", []importModels.CommentImport{settledJira}, false},
		{"a Jira import that can't be dated, re-checked since", []importModels.CommentImport{settledUndated}, false},
		{"a re-checked Jira import, then a Basecamp one before its fix", []importModels.CommentImport{settledJira, of(importModels.ProviderBasecamp, &before)}, true},
		{"no import recorded it", nil, true},
		{"a Jira import before the fix", []importModels.CommentImport{of(importModels.ProviderJira, &before)}, true},
		{"a Jira import after the fix", []importModels.CommentImport{of(importModels.ProviderJira, &after)}, false},
		{"a Jira import that can't be dated", []importModels.CommentImport{of(importModels.ProviderJira, nil)}, true},
		{"a Todoist import before the membership fix", []importModels.CommentImport{of(importModels.ProviderTodoist, &before)}, true},
		{"a Notion import after the membership fix", []importModels.CommentImport{of(importModels.ProviderNotion, &after)}, false},
		{"a Basecamp import before the fix", []importModels.CommentImport{of(importModels.ProviderBasecamp, &before)}, true},
		{"a Linear import while no server has claimed its fix", []importModels.CommentImport{of(importModels.ProviderLinear, &after)}, true},
		{"an Asana import before every fix", []importModels.CommentImport{of(importModels.ProviderAsana, &before)}, false},
		{"an Asana import that can't be dated", []importModels.CommentImport{of(importModels.ProviderAsana, nil)}, false},
		{"an Asana import, then a Jira one before the fix", []importModels.CommentImport{of(importModels.ProviderAsana, &after), of(importModels.ProviderJira, &before)}, true},
		{"a Jira and a Todoist import, both after their fixes", []importModels.CommentImport{of(importModels.ProviderJira, &after), of(importModels.ProviderTodoist, &after)}, false},
	} {
		if got := heldBack(c.in, fixed, settled); got != c.held {
			t.Errorf("%s: held back %v, want %v", c.name, got, c.held)
		}
	}
}

func TestTheRepairHoldsBackTheCommentsOfImportsBeforeTheirFix(t *testing.T) {
	f := newFakeCommentSearch(t)
	fix := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	before, after := fix.Add(-24*time.Hour), fix.Add(24*time.Hour)
	f.fixed[JiraRestrictionsFixed] = fix
	written := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	by := &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: "ada"}
	project := uuid.NewString()
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		f.comment(ids[i], &dgraphStruct.DgraphComment{Text: "<p>A note</p>",
			Task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: project, Name: "Harbor"}}, CommentBy: by, CreatedAt: &written})
	}
	f.importedBy(ids[0], importModels.ProviderJira, &after)
	f.importedBy(ids[1], importModels.ProviderJira, &before)
	f.importedBy(ids[2], "", nil)

	rep, err := RepairTaskCommentSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := TaskCommentSearchRepair{Entries: 4, Rewritten: 2, HeldBack: 2}
	if rep != want {
		t.Errorf("the repair did %+v; want %+v", rep, want)
	}
	if len(f.rewritten) != 2 || f.rewritten[ids[0]] == nil || f.rewritten[ids[3]] == nil {
		t.Errorf("rewrote %v; want only the comment of the Jira import after its fix and the Asana import's", f.rewritten)
	}
}
