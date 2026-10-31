//go:build integration

package business

// An import's statements grow with the tasks it brings by what making each
// takes, and no more (backend audit U1b): every task read its creator and
// assignee twice over Postgres (their mapping and their row) and the graph,
// however many tasks the same people had, and wrote its two mapping rows in
// two statements.
// Run: go test -tags=integration ./business/Import/ -run TestAnImportsTaskStatementsGrowOnlyWithWhatItMakes -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// tasksProvider streams n tasks of one project, made by Maya and assigned
// to Bo or to both of them in turn.
type tasksProvider struct {
	importProvider.Provider
	n int
}

func (tasksProvider) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (tasksProvider) DefaultPriorityMap() map[string]string { return map[string]string{} }
func (p tasksProvider) IterTasksOfProject(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	out, errs := make(chan importProvider.SourceTask), make(chan error)
	go func() {
		defer close(out)
		defer close(errs)
		for i := 0; i < p.n; i++ {
			assignees := []string{"u-bo"}
			if i%2 == 0 {
				assignees = []string{"u-bo", "u-maya"}
			}
			out <- importProvider.SourceTask{SourceID: fmt.Sprintf("t%d", i), ProjectSourceID: "P1", Name: fmt.Sprintf("Task %d", i),
				Status: "todo", CreatedBy: "u-maya", AssigneeIds: assignees, Created: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
		}
	}()
	return out, errs
}

// taskWorld is what an import's task stage needs: a project in Postgres and
// the graph, its team, the person importing, Maya and Bo, and a job of
// provider, run by the person importing, that maps the project as P1 and the
// two people as u-maya and u-bo. Search answers every write.
type taskWorld struct {
	env      *integration.Env
	dg       *integration.DgraphEnv
	job      *importModels.Job
	project  uuid.UUID
	importer *userModels.UserInfo
	// counter, when a test counts Postgres statements, marks importTasks'
	// context, so the import's own statements are what it counts.
	counter *integration.PostgresCounter
}

func newTaskWorld(t *testing.T, provider string) *taskWorld {
	ctx := context.Background()
	env, dg := importEnv(t)

	admin, maya, bo, project, team := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'admin@example.com', NOW(), NOW()), ($2, 'maya@example.com', NOW(), NOW()), ($3, 'bo@example.com', NOW(), NOW())`, []any{admin, maya, bo}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, []any{project, admin}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:admin", "dgraph.type": "User", "user_uuid": admin.String(), "user_name": "admin"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "Maya"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo.String(), "user_name": "Bo"},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Eng"},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch", "project_team": map[string]any{"uid": "_:team"}},
	})

	job := &importModels.Job{Id: uuid.New(), Provider: provider, SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning, TriggeredBy: &admin}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for src, id := range map[string]uuid.UUID{"u-maya": maya, "u-bo": bo} {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, src, id, nil, mustMarshal(map[string]any{}), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityProject, "P1", project, nil, mustMarshal(map[string]any{}), true); err != nil {
		t.Fatal(err)
	}
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: admin}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["admin"], admin.String()
	return &taskWorld{env: env, dg: dg, job: job, project: project, importer: importer}
}

// importTasks runs the task stage's chunk for P1, as a worker claims it,
// with the tasks prov streams.
func (w *taskWorld) importTasks(t *testing.T, prov importProvider.Provider) {
	t.Helper()
	ctx := context.Background()
	if w.counter != nil {
		ctx = w.counter.Context(ctx)
	}
	p1 := "P1"
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: w.job.Id, ChunkType: importModels.ChunkProjectTasks, ParentSourceId: &p1, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if err := processTaskChunk(ctx, prov, w.job, importProvider.JobOptions{}, chunk, w.importer); err != nil {
		t.Fatalf("the project's tasks: %v", err)
	}
}

func importTasksStatements(t *testing.T, n int) int {
	w := newTaskWorld(t, "asana")
	w.counter = integration.CountPostgres(t, w.env)
	w.importTasks(t, tasksProvider{n: n})
	sent := w.counter.Count()
	var made int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, w.project).Scan(&made); err != nil || made != n {
		t.Fatalf("%d tasks made of %d (%v)", made, n, err)
	}
	return sent
}

func TestAnImportsTaskStatementsGrowOnlyWithWhatItMakes(t *testing.T) {
	small, large := 10, 100
	var few, many int
	t.Run("few", func(t *testing.T) { few = importTasksStatements(t, small) })
	t.Run("many", func(t *testing.T) { many = importTasksStatements(t, large) })
	if t.Failed() {
		return
	}
	perTask := float64(many-few) / float64(large-small)
	t.Logf("statements: %d for %d tasks, %d for %d: %.2f a task", few, small, many, large, perTask)
	// Making a task takes its row and its mapping row (with the workspace's).
	if perTask > importTaskStatements {
		t.Errorf("an imported task costs %.2f statements, more than the %.2f making it takes", perTask, importTaskStatements)
	}
}

// importTaskStatements is what making a task takes in Postgres, its row and
// its mapping, and reading whether it was made before (its mapping, the
// workspace's): a person is read once a chunk.
const importTaskStatements = 4.1
