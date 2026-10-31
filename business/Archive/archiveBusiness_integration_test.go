//go:build integration

package business

// archiveBusiness_test.go's tests that need a database, on the harness's
// (tests/integration): Postgres 12 with every migration applied, and Dgraph
// with the production schema.
// Run: go test -tags=integration ./business/Archive/ -run '^(TestARunUnderWayHoldsOffItsOwnTypeOnly|TestGetRecentlyArchivedDocs|TestGetRecentlyArchivedRecordings)$' -v

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	archiveModels "github.com/akashc777/OneCamp/models/postgres/Archive"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A run under way holds off a second run and a restore of its own type, and
// no other type, until it finishes: the guard (checkRunningArchiveJob) reads
// archive_jobs, so two servers never move the same rows at once.
func TestARunUnderWayHoldsOffItsOwnTypeOnly(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	if err := checkRunningArchiveJob(ctx, "posts"); err != nil {
		t.Fatalf("no run is under way, yet posts are held off: %v", err)
	}
	job := uuid.New()
	if err := archiveModels.CreateJob(ctx, job, "posts", time.Now(), nil); err != nil {
		t.Fatalf("start a posts run: %v", err)
	}
	var held *ArchiveAlreadyRunningError
	if _, err := RunArchiveJob(ctx, "posts", nil); !errors.As(err, &held) || held.EntityType != "posts" {
		t.Errorf("a second posts run while one is under way: %v; want ArchiveAlreadyRunningError for posts", err)
	}
	if _, err := RestoreItems(ctx, "posts", []uuid.UUID{uuid.New()}); !errors.As(err, &held) || held.EntityType != "posts" {
		t.Errorf("a posts restore while a posts run is under way: %v; want ArchiveAlreadyRunningError for posts", err)
	}
	if err := checkRunningArchiveJob(ctx, "tasks"); err != nil {
		t.Errorf("a posts run holds off tasks: %v", err)
	}
	if err := archiveModels.FinishJob(ctx, job, "completed", time.Now(), 0, 0, 0, nil); err != nil {
		t.Fatalf("finish the posts run: %v", err)
	}
	if err := checkRunningArchiveJob(ctx, "posts"); err != nil {
		t.Errorf("a finished run still holds off posts: %v", err)
	}
}

// The restore list's graph reads are queries the production schema answers,
// and a graph with nothing deleted lists nothing.
func TestGetRecentlyArchivedDocs(t *testing.T) {
	integration.SetupDgraph(t)
	items, total, err := getRecentlyArchivedDocs(context.Background(), 50, 0, "Document")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
}

func TestGetRecentlyArchivedRecordings(t *testing.T) {
	integration.SetupDgraph(t)
	items, total, err := getRecentlyArchivedRecordings(context.Background(), 50, 0, "Recording")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
}
