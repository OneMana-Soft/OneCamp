//go:build integration

package business

// An import's tables are made by the person it is for, as they are: an
// admin's import acted as a member, since its actor carried the person's id
// alone (tables T-AT; the Notion provider's database too), and canView,
// canManage and the Access Resolver's tableDecision read the admin flag.
// Run: go test -tags=integration ./business/Import/ -run TestAnImportActsAsItsPersonDoes -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnImportActsAsItsPersonDoes(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	ada, maya, gone := uuid.New(), uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{ada: "ada@example.test", maya: "maya@example.test", gone: "gone@example.test"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, email, email); err != nil {
			t.Fatal(err)
		}
	}
	// Ada is an admin; Gone was removed from the workspace after starting an import.
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('ada@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}

	var actors []dataTableBusiness.Actor
	saved := importBase
	t.Cleanup(func() { importBase = saved })
	importBase = func(_ context.Context, a dataTableBusiness.Actor, _ dataTableBusiness.ImportBase) (*dataTableBusiness.ImportedBase, error) {
		actors = append(actors, a)
		return &dataTableBusiness.ImportedBase{Tables: map[string]uuid.UUID{"tblA": uuid.New()}}, nil
	}
	reads := 0
	prov := baseFrom{reads: &reads, make: func() dataTableBusiness.ImportBase {
		return dataTableBusiness.ImportBase{Tables: []dataTableBusiness.ImportTable{{Key: "tblA", Name: "Accounts"}}}
	}}
	// importAs runs a base's chunk of an import person started, from their own
	// workspace: one import at a time runs for a provider and workspace
	// (uq_import_jobs_active_per_provider_workspace), and these stay running.
	importAs := func(person uuid.UUID) error {
		t.Helper()
		job := &importModels.Job{Id: uuid.New(), Provider: "airtable", SourceWorkspaceName: "Acme " + person.String()[:8], Source: "api", Status: "running", TriggeredBy: &person}
		if err := importModels.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		base := "app" + person.String()[:8]
		chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkTableBase, ParentSourceId: &base,
			Status: importModels.ChunkStatusInProgress, MaxAttempts: 5}
		if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
			t.Fatal(err)
		}
		// Whoever pressed Retry is not who the import is for: the actor is the job's person.
		return processTableChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, &userModels.UserInfo{})
	}

	if err := importAs(ada); err != nil {
		t.Fatalf("Ada's import: %v", err)
	}
	if err := importAs(maya); err != nil {
		t.Fatalf("Maya's import: %v", err)
	}
	if len(actors) != 2 {
		t.Fatalf("the importer was asked for %d bases, want 2", len(actors))
	}
	if a := actors[0]; a.UserID != ada || !a.IsAdmin || a.Kind != rowLedger.ActorImport {
		t.Errorf("Ada's import acts as %+v, want her, an admin, as an import", a)
	}
	if a := actors[1]; a.UserID != maya || a.IsAdmin || a.Kind != rowLedger.ActorImport {
		t.Errorf("Maya's import acts as %+v, want her, a member, as an import", a)
	}
	// A person removed since makes nothing.
	if err := importAs(gone); err == nil || len(actors) != 2 {
		t.Errorf("a removed person's import made tables (err %v, %d bases)", err, len(actors))
	}
}
