//go:build integration
// +build integration

package integration_test

// An import waiting to be planned has no plan yet (NULL), and reading it
// failed: it could not be planned, the list of imports failed to load while it
// existed, and its label stayed busy. Both the generic and the Slack screens.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnImportWaitingToBePlannedCanBeRead -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	importDomain "github.com/akashc777/OneCamp/domain/Import"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	slackModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnImportWaitingToBePlannedCanBeRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	asana, slack, planned := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'asana', 'Acme', 'api', 'validating'), ($2, 'slack', 'Acme', 'export_zip', 'validating'),
		       ($3, 'jira', 'Acme', 'api', 'planned')`, asana, slack, planned); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE import_jobs SET plan = '{"task_count": 7}' WHERE id = $1`, planned); err != nil {
		t.Fatal(err)
	}

	j, err := importModels.GetJob(ctx, asana)
	if err != nil || j == nil || j.Plan != nil {
		t.Fatalf("a job waiting to be planned: %+v %v", j, err)
	}
	if j, err := importModels.GetJob(ctx, planned); err != nil || string(j.Plan) != `{"task_count": 7}` {
		t.Fatalf("a planned job keeps its plan: %+v %v", j, err)
	}
	if jobs, err := importDomain.ListJobs(ctx, "", 50); err != nil || len(jobs) != 3 {
		t.Fatalf("the list of imports: %d %v", len(jobs), err)
	}
	if j, err := slackModels.GetJob(ctx, slack); err != nil || j == nil || j.Plan != nil {
		t.Fatalf("an uploaded Slack export waiting to be planned: %+v %v", j, err)
	}
	if jobs, err := slackModels.ListJobs(ctx, 50); err != nil || len(jobs) != 1 {
		t.Fatalf("the list of Slack imports: %d %v", len(jobs), err)
	}
}
