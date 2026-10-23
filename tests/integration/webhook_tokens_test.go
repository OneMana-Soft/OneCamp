//go:build integration
// +build integration

package integration_test

// Incoming webhook tokens against real Postgres 12 (audit A1): only a
// token's hash is kept, so a copy of the table posts as no webhook; a token
// given out before that keeps posting, through the minute the old server runs
// on the new schema and the new one's start; and the migration goes down and
// up again.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestAnIncomingWebhookOnlyPostsMessages|TestWebhookTokensMigrationUpDownUp)$' -v

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	linkSecretModel "github.com/akashc777/OneCamp/models/postgres/LinkSecret"
	"github.com/akashc777/OneCamp/tests/integration"
)

func webhookTokensMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_webhook_token_hash."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one webhook_token_hash %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// webhooksHolding is how many webhooks hold text anywhere in their row, where
// a copy of the table would find it.
func webhooksHolding(t *testing.T, db *sql.DB, text string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhooks w WHERE strpos(w::text, $1) > 0`, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The migration only adds, so the server from before keeps working on it
// until the new one starts: it finds a webhook by its token, gives it a new
// one, and makes another. The new server's start hashes what it wrote, so the
// tokens it last gave out post and the one it replaced doesn't. Going down
// puts no token back in the clear: each is made again.
func TestWebhookTokensMigrationUpDownUp(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	up, down := webhookTokensMigration(t, "up"), webhookTokensMigration(t, "down")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	owner := uuid.New()
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'ci', 'CI', NOW(), NOW())`,
		owner, owner.String()+"@example.test")
	token := func() string { return strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")[:43] }
	posts := func(token string) bool {
		_, err := webhookBusiness.ProcessIncomingWebhook(ctx, token)
		return err == nil
	}
	exec(down)
	// Alerts' token as the server from before gave it out, kept as it is.
	given, alerts := token(), uuid.New()
	exec(`INSERT INTO webhooks (id, name, type, token, created_by, bot_name) VALUES ($1, 'Alerts', 'incoming', $2, $3, 'Alerts')`, alerts, given, owner)
	exec(up)

	// The old server, still running: its own statements.
	var found uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM webhooks WHERE token = $1 AND deleted_at IS NULL`, given).Scan(&found); err != nil || found != alerts {
		t.Fatalf("the old server looking Alerts up: %s, %v", found, err)
	}
	again, deploys := token(), token()
	exec(`UPDATE webhooks SET token = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`, again, alerts)
	exec(`INSERT INTO webhooks (id, name, type, token, created_by, bot_name) VALUES ($1, 'Deploys', 'incoming', $2, $3, 'Deploys')`, uuid.New(), deploys, owner)

	// The new server starts.
	if err := linkSecretModel.HashLeftInClear(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{given, again, deploys} {
		if n := webhooksHolding(t, env.PG, tok); n != 0 {
			t.Errorf("%d webhooks keep a token in the clear", n)
		}
	}
	if !posts(again) || !posts(deploys) {
		t.Errorf("the tokens the old server gave out: Alerts' new one posts %v, Deploys' %v", posts(again), posts(deploys))
	}
	if posts(given) {
		t.Error("Alerts' first token still posts after it was made again")
	}
	exec(down)
	if n := webhooksHolding(t, env.PG, again); n != 0 {
		t.Errorf("down: %d webhooks hold a token again", n)
	}
	exec(up)
	if posts(again) {
		t.Error("up again: a token whose hash went down posts")
	}
}
