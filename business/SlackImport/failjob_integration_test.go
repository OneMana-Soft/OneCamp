//go:build integration
// +build integration

package business

// A Slack import that fails keeps no URL's query string in what it logs,
// stores or broadcasts: a file's private URL carries its token in one.
//
// Run: go test -tags=integration ./business/SlackImport/ -run TestAFailedSlackImportKeepsNoURLQuery -v

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAFailedSlackImportKeepsNoURLQuery(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	var logged bytes.Buffer
	prev := helpers.Logger
	helpers.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	t.Cleanup(func() { helpers.Logger = prev })

	job := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'running')`, job); err != nil {
		t.Fatal(err)
	}
	failJob(ctx, job, "Acme", errors.New(`Get "https://files.slack.com/files-pri/T1-F1/report.pdf?t=xoxe-123456": dial tcp: i/o timeout`))

	var stored string
	if err := env.PG.QueryRow(`SELECT error_message FROM import_jobs WHERE id = $1`, job).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for where, s := range map[string]string{"the log": logged.String(), "the job's error": stored} {
		if strings.Contains(s, "xoxe-123456") {
			t.Errorf("%s keeps the token: %s", where, s)
		}
		if !strings.Contains(s, "files.slack.com/files-pri/T1-F1/report.pdf") {
			t.Errorf("%s lost which file: %s", where, s)
		}
	}
}
