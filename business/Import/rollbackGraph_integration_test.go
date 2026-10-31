//go:build integration

package business

// Package 24 (the 30-day audit, C8): a rollback marked what an import made
// deleted in Postgres alone. A project's board and lists read the graph, and
// search keeps its own copy, so the import's tasks, subtasks, projects and
// teams stayed on both.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestARolledBackImportTakesWhatItMadeOffTheGraphAndSearch$|^TestARollbacksStatementsDoNotGrowWithItsTasks$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// subtasksProvider is tasksProvider whose tasks have subtasks each.
type subtasksProvider struct {
	tasksProvider
	subtasks int
}

func (p subtasksProvider) IterSubtasksOfTask(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, parent string) (<-chan importProvider.SourceTask, <-chan error) {
	out, errs := make(chan importProvider.SourceTask), make(chan error)
	go func() {
		defer close(out)
		defer close(errs)
		for i := 0; i < p.subtasks; i++ {
			out <- importProvider.SourceTask{SourceID: fmt.Sprintf("%s-%d", parent, i), Name: fmt.Sprintf("Step %d", i),
				Status: "todo", CreatedBy: "u-maya", Created: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)}
		}
	}()
	return out, errs
}

// importSubtasks runs the subtask stage's chunk for the task parent, as a
// worker claims it.
func (w *taskWorld) importSubtasks(t *testing.T, prov importProvider.Provider, parent string) {
	t.Helper()
	ctx := context.Background()
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: w.job.Id, ChunkType: importModels.ChunkTaskSubtasks, ParentSourceId: &parent, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if err := processSubtaskChunk(ctx, prov, w.job, importProvider.JobOptions{}, chunk, w.importer); err != nil {
		t.Fatalf("the subtasks of %s: %v", parent, err)
	}
}

// made is what the job made of kind, by its mappings.
func (w *taskWorld) made(t *testing.T, kind string) []string {
	t.Helper()
	rows, err := w.env.PG.Query(`SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = $2 AND created_by_this_import`, w.job.Id, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// graphKind is how the graph names a kind's nodes and their deletion time.
type graphKind struct{ id, deleted string }

var (
	taskNodes    = graphKind{"task_uuid", "task_deleted_at"}
	projectNodes = graphKind{"project_uuid", "project_deleted_at"}
	teamNodes    = graphKind{"team_uuid", "team_deleted_at"}
)

// inGraph is how many of ids the graph has, and how many of those it has
// deleted: a deletion time after the epoch, since a live node's is the zero
// time (0001-01-01).
func inGraph(t *testing.T, dg *integration.DgraphEnv, k graphKind, ids []string) (has, deleted int) {
	t.Helper()
	list, _ := json.Marshal(ids)
	type count struct {
		N int `json:"count"`
	}
	var out struct {
		Has     []count `json:"has"`
		Deleted []count `json:"deleted"`
	}
	q := fmt.Sprintf(`{ has(func: eq(%[1]s, %[3]s)) { count(uid) } deleted(func: eq(%[1]s, %[3]s)) @filter(gt(%[2]s, "1970-01-01T00:00:00Z")) { count(uid) } }`, k.id, k.deleted, list)
	if err := json.Unmarshal(dg.Query(t, q), &out); err != nil || len(out.Has) != 1 || len(out.Deleted) != 1 {
		t.Fatalf("read %s of %v: %v", k.id, ids, err)
	}
	return out.Has[0].N, out.Deleted[0].N
}

// inSearch is whether search holds index's entry id, and whether it is marked
// deleted: a deletion date of 1 or after, since below 1 was never a deletion.
func inSearch(ctx context.Context, index, id string) (held, deleted bool) {
	resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
	if err != nil || !resp.Found {
		return false, false
	}
	var entry struct {
		Deleted *float64 `json:"deleted_date"`
	}
	if json.Unmarshal(resp.Source, &entry) != nil {
		return true, false
	}
	return true, entry.Deleted != nil && *entry.Deleted >= 1
}

// searchEntry is a search entry's deletion: its time, and whose mark it is.
type searchEntry struct {
	By      string  `json:"deleted_by"`
	Deleted float64 `json:"deleted_date"`
}

// out is whether search leaves the entry out: a deletion time of 1 or after,
// since below 1 was never a deletion.
func (e searchEntry) out() bool { return e.Deleted >= 1 }

// searchEntries is each search entry naming a comment, as HideComment marks
// them: its own entry, its files' and its AI entry, each matched whole (by
// the field or its .keyword), in whichever of those indexes the install has.
func searchEntries(ctx context.Context, comment string) ([]searchEntry, error) {
	var should []map[string]any
	for _, field := range []string{"comment_id", "attachment_comment_id", "content_uuid"} {
		for _, f := range []string{field, field + ".keyword"} {
			should = append(should, map[string]any{"term": map[string]any{f: comment}})
		}
	}
	body, err := json.Marshal(map[string]any{"size": 100,
		"query": map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1}}})
	if err != nil {
		return nil, err
	}
	ignore := true
	resp, err := opensearchInit.OpenSearchClient.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{"comments", "attachments", "ai_embeddings"}, Body: bytes.NewReader(body),
		Params: opensearchapi.SearchParams{IgnoreUnavailable: &ignore}})
	if err != nil {
		return nil, err
	}
	entries := make([]searchEntry, len(resp.Hits.Hits))
	for i, h := range resp.Hits.Hits {
		if err := json.Unmarshal(h.Source, &entries[i]); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// within waits for cond, and fails the test with what it waited for when
// cond doesn't come true in time.
func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestARolledBackImportTakesWhatItMadeOffTheGraphAndSearch(t *testing.T) {
	ctx := context.Background()
	w := newTaskWorld(t, "asana")
	// Search is a real one here, asked what it holds rather than what it was
	// sent.
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	// The import made a team, and a project in it that it brought nothing to.
	if err := resolveOneTeam(ctx, w.job, &importProvider.SourceTeam{SourceID: "T2", Name: "Ops"}, w.importer); err != nil {
		t.Fatal(err)
	}
	if err := resolveOneProject(ctx, w.job, &importProvider.SourceProject{SourceID: "P2", Name: "Roadmap", TeamSourceID: "T2"}, w.importer); err != nil {
		t.Fatal(err)
	}
	prov := subtasksProvider{tasksProvider{n: 3}, 2}
	w.importTasks(t, prov)
	w.importSubtasks(t, prov, "t0")
	// Someone's own task keeps the import's first project, so its tasks leave
	// search by their own step, not by their project's.
	if _, err := w.env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, uuid.New(), w.project, w.importer.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if err := importModels.UpdateStatus(ctx, w.job.Id, importModels.StatusCompleted, nil, nil); err != nil {
		t.Fatal(err)
	}

	tasks := append(w.made(t, importModels.EntityTask), w.made(t, importModels.EntitySubtask)...)
	projects, teams := w.made(t, importModels.EntityProject), w.made(t, importModels.EntityTeam)
	if len(tasks) != 5 || len(projects) != 2 || len(teams) != 1 {
		t.Fatalf("made %d tasks and subtasks of 5, %d projects of 2, %d teams of 1", len(tasks), len(projects), len(teams))
	}
	roadmap, ops := "", teams[0]
	for _, p := range projects {
		if p != w.project.String() {
			roadmap = p
		}
	}
	type entry struct{ index, id string }
	var entries []entry
	for _, id := range tasks {
		entries = append(entries, entry{openSearchStruct.TASK_INDEX, id})
	}
	entries = append(entries, entry{"projects", roadmap}, entry{openSearchStruct.TEAM_INDEX, ops})
	for _, e := range entries {
		within(t, fmt.Sprintf("search holds %s %s, live", e.index, e.id), func() bool {
			held, deleted := inSearch(ctx, e.index, e.id)
			return held && !deleted
		})
	}
	// An import's entries are searchable long before anyone rolls it back,
	// and an archive finds the entries it marks by a search, which sees an
	// entry only once its index is refreshed.
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{openSearchStruct.TASK_INDEX, "projects", openSearchStruct.TEAM_INDEX}}); err != nil {
		t.Fatal(err)
	}
	nodes := []struct {
		what string
		kind graphKind
		ids  []string
	}{
		{"tasks and subtasks", taskNodes, tasks},
		{"projects", projectNodes, []string{roadmap}},
		{"teams", teamNodes, []string{ops}},
	}
	for _, n := range nodes {
		if has, deleted := inGraph(t, w.dg, n.kind, n.ids); has != len(n.ids) || deleted != 0 {
			t.Fatalf("before the rollback the graph has %d of the import's %d %s, %d of them deleted", has, len(n.ids), n.what, deleted)
		}
	}

	if err := RollbackImport(ctx, w.job.Id); err != nil {
		t.Fatal(err)
	}

	// The board and the lists read the graph: each is deleted there.
	for _, n := range nodes {
		if has, deleted := inGraph(t, w.dg, n.kind, n.ids); has != len(n.ids) || deleted != len(n.ids) {
			t.Errorf("after the rollback %d of the import's %d %s are still live in the graph (%d there)", has-deleted, len(n.ids), n.what, has)
		}
	}
	if _, deleted := inGraph(t, w.dg, projectNodes, []string{w.project.String()}); deleted != 0 {
		t.Errorf("the project with someone's own task in it was deleted from the graph")
	}
	// Search is told off the request.
	for _, e := range entries {
		within(t, fmt.Sprintf("search marks %s %s deleted", e.index, e.id), func() bool {
			_, deleted := inSearch(ctx, e.index, e.id)
			return deleted
		})
	}
}

// A rollback's statements don't grow with the tasks it takes away. A task's
// own delete wrote its row, the graph and search for each, so a large
// import's rollback ran past the request's time.
func TestARollbacksStatementsDoNotGrowWithItsTasks(t *testing.T) {
	small, large := 10, 100
	var few, many int
	t.Run("few", func(t *testing.T) { few = rollbackStatements(t, small) })
	t.Run("many", func(t *testing.T) { many = rollbackStatements(t, large) })
	if t.Failed() {
		return
	}
	perTask := float64(many-few) / float64(large-small)
	t.Logf("statements: %d to roll back %d tasks, %d for %d: %.2f a task", few, small, many, large, perTask)
	// Below one a task rather than none: what a rollback starts in the
	// background (its project's memory) may land in either count.
	if perTask >= 0.5 {
		t.Errorf("rolling back a task costs %.2f statements; a rollback's own are the same for any number of tasks", perTask)
	}
}

// rollbackStatements is how many statements rolling back an import of n
// tasks sends Postgres, once it has taken them off the graph.
func rollbackStatements(t *testing.T, n int) int {
	ctx := context.Background()
	w := newTaskWorld(t, "asana")
	w.importTasks(t, tasksProvider{n: n})
	if err := importModels.UpdateStatus(ctx, w.job.Id, importModels.StatusCompleted, nil, nil); err != nil {
		t.Fatal(err)
	}
	counter := integration.CountPostgres(t, w.env)
	if err := RollbackImport(counter.Context(ctx), w.job.Id); err != nil {
		t.Fatal(err)
	}
	sent := counter.Count()
	if has, deleted := inGraph(t, w.dg, taskNodes, w.made(t, importModels.EntityTask)); has != n || deleted != n {
		t.Fatalf("the graph has %d of the %d tasks, %d deleted", has, n, deleted)
	}
	return sent
}
