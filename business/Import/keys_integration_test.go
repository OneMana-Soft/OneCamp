//go:build integration

package business

// I1: an import keeps the source's keys. The project takes the source's key
// (ENG) when it's free, and each task its number (ENG-7 stays 7) when the
// project hasn't used it; numbers made afterwards follow the highest.
// Run: PKGS=./business/Import integ_rel.sh <wt> <log> 'TestImportedKeys'

import (
	"context"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestImportedKeys(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "jira", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	by, imported, other := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'keys@example.com', NOW(), NOW())`, by)
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Engine', $3), ($2, 'Other', $3)`, imported, other, by)
	// Another project already has OPS.
	if _, _, err := taskKeys.SetKey(ctx, other, "OPS", by); err != nil {
		t.Fatal(err)
	}

	keepSourceKey(ctx, job, &importProvider.SourceProject{SourceID: "ENG", SourceKey: "ENG"}, imported, by)
	if key, _, _, err := taskKeys.Of(ctx, imported); err != nil || key != "ENG" {
		t.Fatalf("the imported project's key %q (%v), want ENG", key, err)
	}

	numbered := func(n int) int {
		t.Helper()
		got, err := taskKeyModel.InsertTaskNumbered(ctx, uuid.New(), imported, by, time.Now(), n)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if a, b := numbered(7), numbered(3); a != 7 || b != 3 {
		t.Fatalf("ENG-7 and ENG-3 became %d and %d", a, b)
	}
	// Seven is used: the next free number after the highest.
	if c := numbered(7); c != 8 {
		t.Fatalf("a second 7 became %d, want 8", c)
	}
	if d, err := taskKeyModel.InsertTask(ctx, uuid.New(), imported, by, time.Now()); err != nil || d != 9 {
		t.Fatalf("a task made after them is %d (%v), want 9", d, err)
	}

	// A key another project has stays theirs; the import says so.
	keepSourceKey(ctx, job, &importProvider.SourceProject{SourceID: "OPS", SourceKey: "OPS"}, uuid.New(), by)
	var said int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM import_errors WHERE import_id = $1 AND code = 'KEY_NOT_KEPT'`, job.Id).Scan(&said); err != nil || said != 1 {
		t.Fatalf("a taken key said %d times (%v), want once", said, err)
	}
	if key, _, _, _ := taskKeys.Of(ctx, other); key != "OPS" {
		t.Fatalf("the other project's key moved: %q", key)
	}
}
