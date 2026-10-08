//go:build integration

package business

// Where an imported task landed, against a real Postgres with the import tables.
// Run: go test -tags=integration ./business/Import/ -run TestLookupMappedTask -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A subtask's own subtask (ClickUp and Linear nest them) finds its way to the
// top-level task, and its comments and files find the subtask they belong to.
func TestLookupMappedTask(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	top, sub := uuid.New(), uuid.New()
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityTask, "A", top, nil,
		mustMarshal(map[string]any{"project_source_id": "P"}), true); err != nil {
		t.Fatal(err)
	}
	parent := "A"
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntitySubtask, "B", sub, &parent,
		mustMarshal(map[string]any{"parent_task_uuid": top.String(), "parent_source_id": "A", "project_source_id": "P"}), true); err != nil {
		t.Fatal(err)
	}

	if m := lookupMappedTask(ctx, job.Id, "A"); m.UUID != top || m.TopUUID != top || m.TopSourceID != "A" || m.ProjectSourceID != "P" {
		t.Fatalf("a top-level task is its own top: %+v", m)
	}
	if m := lookupMappedTask(ctx, job.Id, "B"); m.UUID != sub || m.TopUUID != top || m.TopSourceID != "A" || m.ProjectSourceID != "P" {
		t.Fatalf("a subtask is found as itself, under its top-level task, in its project: %+v", m)
	}
	if m := lookupMappedTask(ctx, job.Id, "C"); m.UUID != uuid.Nil {
		t.Fatalf("a task not imported yet isn't found: %+v", m)
	}
}

// A warning an import logs is kept, and listed for the person running it.
func TestImportErrorsAreKept(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "trello", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	importModels.LogImportError(ctx, job.Id, nil, importModels.EntityTask, "card-1", importModels.SeverityWarning, "TASK_IMPORT_FAILED", "the card couldn't be read", nil)
	rows, err := importModels.ListErrors(ctx, job.Id, importModels.SeverityWarning, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || *(rows[0]["source_id"].(*string)) != "card-1" || rows[0]["message"] != "the card couldn't be read" {
		t.Fatalf("the warning, with its source id: %+v", rows)
	}
}
