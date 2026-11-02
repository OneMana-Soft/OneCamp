//go:build integration

package business

// An import's tasks reach the Work Change Log (WCL-1) as made, as tasks made
// in the app do, once a written batch: a board open on the project shows
// them as they come, and a reader catching up from the log has them. Before,
// the importer wrote its tasks straight to the stores and logged none
// (CR-121's sweep of every task writer).
// Run: go test -tags=integration ./business/Import/ -run TestAnImportsTasksAreLoggedAsMadeOnceABatch -v

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// stepsProvider is tasksProvider with two sub-tasks, steps, under any task,
// and its first task, t0, one its source keeps to some people: t0 comes in
// restricted, and so do the steps under it (they inherit its reason).
type stepsProvider struct{ tasksProvider }

func (p stepsProvider) IterTasksOfProject(ctx context.Context, job *importModels.Job, opts importProvider.JobOptions, project string) (<-chan importProvider.SourceTask, <-chan error) {
	tasks, errs := p.tasksProvider.IterTasksOfProject(ctx, job, opts, project)
	out := make(chan importProvider.SourceTask)
	go func() {
		defer close(out)
		for st := range tasks {
			if st.SourceID == "t0" {
				st.Restricted = "a private task"
			}
			out <- st
		}
	}()
	return out, errs
}

func (stepsProvider) IterSubtasksOfTask(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, parent string) (<-chan importProvider.SourceTask, <-chan error) {
	out, errs := make(chan importProvider.SourceTask), make(chan error)
	go func() {
		defer close(out)
		defer close(errs)
		for i := 0; i < 2; i++ {
			out <- importProvider.SourceTask{SourceID: fmt.Sprintf("%s-step%d", parent, i), ParentTaskID: parent, ProjectSourceID: "P1",
				Name: fmt.Sprintf("Step %d", i), Status: "todo", CreatedBy: "u-maya", AssigneeIds: []string{"u-bo"},
				Created: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)}
		}
	}()
	return out, errs
}

// importSteps runs the sub-tasks chunk of the task parent, as a worker claims
// it, in the context RunImport gives its stages (importContext): its reads of
// the parent are the system's, so a restricted parent is read too.
func (w *taskWorld) importSteps(t *testing.T, prov importProvider.Provider, parent string) {
	t.Helper()
	ctx := importContext(context.Background())
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: w.job.Id, ChunkType: importModels.ChunkTaskSubtasks, ParentSourceId: &parent, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if err := processSubtaskChunk(ctx, prov, w.job, importProvider.JobOptions{}, chunk, w.importer); err != nil {
		t.Fatalf("the task's steps: %v", err)
	}
}

// placeByEmail makes the world's person src (u-maya or u-bo) one matched by
// their address, whom the import places: a task they're given is theirs. The
// world maps both by name alone, so their tasks come in unassigned.
func (w *taskWorld) placeByEmail(t *testing.T, src string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.env.PG.QueryRow(`UPDATE import_id_map SET metadata = '{"matched_by": "email"}'
		WHERE import_id = $1 AND entity_type = $2 AND source_id = $3 RETURNING onecamp_uuid`,
		w.job.Id, importModels.EntityUser, src).Scan(&id); err != nil {
		t.Fatalf("placing %s: %v", src, err)
	}
	return id
}

func TestAnImportsTasksAreLoggedAsMadeOnceABatch(t *testing.T) {
	w := newTaskWorld(t, "asana")
	bo := w.placeByEmail(t, "u-bo") // every task and step is Bo's
	prov := stepsProvider{tasksProvider{n: 30}}
	w.importTasks(t, prov)
	w.importSteps(t, prov, "t0") // t0's steps, restricted as t0 is
	t0, _ := importModels.LookupIdMapping(context.Background(), w.job.Id, importModels.EntityTask, "t0")
	if t0 == uuid.Nil {
		t.Fatal("t0 wasn't imported")
	}

	rows, err := w.env.PG.Query(`SELECT kind, changed_at, COALESCE(actor_uuid::text, ''), restricted, COALESCE(diff->>'task_name', ''),
		COALESCE(diff->'task_parent_task'->>'task_uuid', ''), COALESCE(diff->'task_assignee'->>'user_uuid', '')
		FROM task_changes WHERE project_uuid::text = $1 ORDER BY sequence`, w.project.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names, batches []string
	var sizes []int
	var last time.Time
	for rows.Next() {
		var kind, actor, name, parent, assignee string
		var at time.Time
		var restricted bool
		if err := rows.Scan(&kind, &at, &actor, &restricted, &name, &parent, &assignee); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		if kind != "task.created" || actor != w.importer.UserDgraphInfo.Uuid {
			t.Errorf("%s logged as %s by %q: want task.created by the person importing", name, kind, actor)
		}
		// Task 0 and its two steps are restricted, the other 29 open. A
		// batch's open list names only the open ones, so a restricted task
		// is never logged open: a board shows it only to its people.
		if want := name == "Task 0" || strings.HasPrefix(name, "Step"); restricted != want {
			t.Errorf("%s logged restricted %v, want %v: only Task 0 and the steps under it are kept to some people", name, restricted, want)
		}
		if assignee != bo.String() {
			t.Errorf("%s logged assigned to %q, want Bo (%s): a board shows who has it", name, assignee, bo)
		}
		wantParent := ""
		if strings.HasPrefix(name, "Step") {
			wantParent = t0.String()
		}
		if parent != wantParent {
			t.Errorf("%s logged under %q, want %q", name, parent, wantParent)
		}
		// One RecordTasks stamps its batch with one time.
		if len(sizes) == 0 || !at.Equal(last) {
			sizes, batches = append(sizes, 0), append(batches, name)
		}
		sizes[len(sizes)-1]++
		last = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, 32)
	for i := 0; i < 30; i++ {
		want = append(want, fmt.Sprintf("Task %d", i))
	}
	want = append(want, "Step 0", "Step 1")
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("the log has %d of the import's %d tasks made: %v", len(names), len(want), names)
	}
	// The chunk's batches of 25 and what's left, then the steps' chunk: one log each.
	if fmt.Sprint(sizes) != "[25 5 2]" {
		t.Errorf("logged in batches of %v (starting at %v), want [25 5 2]: once for each written batch", sizes, batches)
	}
}
