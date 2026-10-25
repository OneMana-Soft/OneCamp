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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
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

func importTasksStatements(t *testing.T, n int) int {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

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

	job := &importModels.Job{Id: uuid.New(), Provider: "asana", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
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
	p1 := "P1"
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkProjectTasks, ParentSourceId: &p1, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: admin}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["admin"], admin.String()

	counter := integration.CountPostgres(t, env)
	if err := processTaskChunk(counter.Context(ctx), tasksProvider{n: n}, job, importProvider.JobOptions{}, chunk, importer); err != nil {
		t.Fatalf("import %d tasks: %v", n, err)
	}
	sent := counter.Count()
	var made int
	if err := env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, project).Scan(&made); err != nil || made != n {
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
