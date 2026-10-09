//go:build integration
// +build integration

package integration_test

// A Slack import's status changes are saved. The statement saving them could
// not be prepared (its status parameter read as varchar in one place and text
// in another), so no Slack import could be finalised, started, moved between
// stages, finished or cancelled.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackImportsStatusChangesAreSaved -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	slackModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASlackImportsStatusChangesAreSaved(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'validating')`, id); err != nil {
		t.Fatal(err)
	}
	stage, msg := "users", "export is corrupt"
	if err := slackModels.UpdateStatus(ctx, id, slackModels.StatusRunning, &stage, nil); err != nil {
		t.Fatalf("starting: %v", err)
	}
	j, err := slackModels.GetJob(ctx, id)
	if err != nil || j.Status != "running" || j.StartedAt == nil || j.Stage == nil || *j.Stage != "users" || j.CompletedAt != nil {
		t.Fatalf("started: %+v %v", j, err)
	}
	if err := slackModels.UpdateStatus(ctx, id, slackModels.StatusFailed, nil, &msg); err != nil {
		t.Fatalf("failing: %v", err)
	}
	if j, err = slackModels.GetJob(ctx, id); err != nil || j.Status != "failed" || j.CompletedAt == nil || j.ErrorMessage == nil || *j.ErrorMessage != msg {
		t.Fatalf("failed: %+v %v", j, err)
	}
}
