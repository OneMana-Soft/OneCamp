//go:build integration
// +build integration

package integration_test

// An import's error never keeps a URL's query string. A provider's network
// error carries its request's URL, Trello's with the key and token in it, and
// the import stored that as the job's error, a chunk's error and an error row,
// all of which the admin's screens show.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnImportsErrorsKeepNoURLQuery -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	slackModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnImportsErrorsKeepNoURLQuery(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	trello := `Get "https://api.trello.com/1/boards/b1/cards?key=0123abcd&token=ATTA9876": dial tcp: lookup api.trello.com: no such host`
	leaks := func(where, s string) {
		t.Helper()
		if strings.Contains(s, "key=") || strings.Contains(s, "ATTA9876") {
			t.Errorf("%s keeps the credentials: %s", where, s)
		}
		if !strings.Contains(s, "api.trello.com/1/boards/b1/cards") {
			t.Errorf("%s lost which call failed: %s", where, s)
		}
	}

	job, slackJob, chunk := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'trello', 'Acme', 'api', 'running'), ($2, 'slack', 'Beta', 'export_zip', 'running')`, job, slackJob); err != nil {
		t.Fatal(err)
	}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{{Id: chunk, ImportId: job, ChunkType: "tasks", Status: "pending", MaxAttempts: 3}}); err != nil {
		t.Fatal(err)
	}

	// The job's error.
	if err := importModels.UpdateStatus(ctx, job, importModels.StatusFailed, nil, &trello); err != nil {
		t.Fatal(err)
	}
	var jobErr string
	if err := env.PG.QueryRow(`SELECT error_message FROM import_jobs WHERE id = $1`, job).Scan(&jobErr); err != nil {
		t.Fatal(err)
	}
	leaks("the job's error", jobErr)

	// A chunk's error, failed and put back for a retry.
	for what, write := range map[string]func() error{
		"a failed chunk":   func() error { return importModels.FailChunk(ctx, chunk, 0, nil, trello) },
		"a chunk put back": func() error { return importModels.ResetChunkForRetry(ctx, chunk, trello) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
		var chunkErr string
		if err := env.PG.QueryRow(`SELECT error FROM import_chunks WHERE id = $1`, chunk).Scan(&chunkErr); err != nil {
			t.Fatal(err)
		}
		leaks(what, chunkErr)
	}

	// An error row, from either kind of import, with the URL in its context too.
	detail, _ := json.Marshal(map[string]string{"url": "https://api.trello.com/1/boards/b1/cards?key=0123abcd&token=ATTA9876"})
	importModels.LogImportError(ctx, job, nil, "board", "b1", "error", "FETCH_FAILED", trello, detail)
	slackModels.LogImportError(ctx, slackJob, nil, "file", "F1", "error", "DOWNLOAD_FAILED", trello, detail)
	rows, err := env.PG.Query(`SELECT message, context::text FROM import_errors`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var message, context string
		if err := rows.Scan(&message, &context); err != nil {
			t.Fatal(err)
		}
		leaks("an error row", message)
		leaks("an error row's context", context)
		n++
	}
	if n != 2 {
		t.Errorf("%d error rows, want 2", n)
	}
}
