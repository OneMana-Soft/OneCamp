//go:build integration

package business

// Rolling back what an import brought in, against a real Postgres with every
// migration applied.
// Run: go test -tags=integration ./business/Import/ -run TestRollingBackAnImportedFile -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// An imported file is removed with the rest of the import. The statement
// that removed it set a column the attachments table doesn't have, so it
// failed, and with it every rollback of an import that brought a file.
func TestRollingBackAnImportedFileRemovesIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "trello", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusCompleted}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	file, task := uuid.New(), uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO attachments (id, obj_key, src_value, src_key) VALUES ($1, 'k', $2, 'task')`, file, task.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO tasks (id) VALUES ($1)`, task); err != nil {
		t.Fatal(err)
	}
	for entity, id := range map[string]uuid.UUID{importModels.EntityFile: file, importModels.EntityTask: task} {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, entity, "s-"+id.String()[:8], id, nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := softDelete(ctx, tx, job.Id, importModels.EntityFile, "attachments"); err != nil {
		t.Fatalf("removing the imported file: %v", err)
	}
	if err := softDelete(ctx, tx, job.Id, importModels.EntityTask, "tasks"); err != nil {
		t.Fatalf("removing the imported task: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for table, id := range map[string]uuid.UUID{"attachments": file, "tasks": task} {
		var gone bool
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
			`SELECT deleted_at IS NOT NULL FROM `+table+` WHERE id = $1`, id).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		if !gone {
			t.Errorf("the imported row in %s is still there", table)
		}
	}
}
