//go:build integration

package business

// A run whose write fails names nothing as archived: a run takes what it
// names out of search even when it fails (a doc run that fails part way has
// archived the docs its earlier batches moved), so naming the rows it only
// meant to archive would hide live posts and files. Against a real Postgres 12
// with every migration applied.
// Run: go test -tags=integration ./business/Archive/ -run TestARunWhoseWriteFailsNamesNothingArchived -v

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestARunWhoseWriteFailsNamesNothingArchived(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	old := time.Now().AddDate(-1, -1, 0)
	if _, err := env.PG.Exec(`INSERT INTO posts (id, created_at, updated_at) VALUES ($1, $2, $2)`, uuid.New(), old); err != nil {
		t.Fatalf("seed a post: %v", err)
	}
	if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_value, src_key, created_at) VALUES ($1, 'k', 'v', 'post', $2)`, uuid.New(), old); err != nil {
		t.Fatalf("seed a file: %v", err)
	}
	// Every archive write is refused.
	if _, err := env.PG.Exec(`CREATE FUNCTION refuse_archive() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"posts", "attachments"} {
		if _, err := env.PG.Exec(`CREATE TRIGGER refuse_archive BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE PROCEDURE refuse_archive()`); err != nil {
			t.Fatal(err)
		}
	}

	for name, run := range map[string]func(context.Context, time.Time) (int64, int64, []string, error){
		"posts": archivePosts, "attachments": archiveAttachments,
	} {
		archived, attempted, ids, err := run(ctx, time.Now().AddDate(-1, 0, 0))
		if err == nil || archived != 0 || attempted != 1 || len(ids) != 0 {
			t.Errorf("%s, its write refused: archived %d of %d, named %v, error %v; want an error and none archived or named", name, archived, attempted, ids, err)
		}
	}
}
