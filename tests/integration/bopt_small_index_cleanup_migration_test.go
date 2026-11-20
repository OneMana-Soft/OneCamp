//go:build integration

package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// What 606 drops, each as Postgres writes back the definition of the
// migration that made it: the down has to bring back exactly this.
var boptSmallIndexes = map[string]string{
	"poll_votes_poll_idx":                       "ON public.poll_votes USING btree (poll_id)",
	"idx_archive_job_items_job":                 "ON public.archive_job_items USING btree (job_id)",
	"idx_users_notification_preferences_token":  "ON public.users_notification_preferences USING btree (unsubscribe_token)",
	"idx_notification_email_suppressions_email": "ON public.notification_email_suppressions USING btree (email)",
	"user_recovery_codes_user_idx":              "ON public.user_recovery_codes USING btree (user_id)",
	"idx_github_links_project_id":               "ON public.github_links USING btree (project_id) WHERE (deleted_at IS NULL)",
	"idx_webhooks_token":                        "ON public.webhooks USING btree (token) WHERE (deleted_at IS NULL)",
	"idx_users_signup_method":                   "ON public.users USING btree (signup_method)",
	"idx_users_github_login":                    "ON public.users USING btree (github_login) WHERE (github_login IS NOT NULL)",
	// Only where 52 kept a non-empty legacy table; the test makes one.
	"idx_github_reaction_refs_task":   "ON public.github_reaction_refs_legacy_38 USING btree (task_id)",
	"idx_github_reaction_refs_lookup": "ON public.github_reaction_refs_legacy_38 USING btree (task_id, user_id, github_name)",
	// webhook_commands (46), back with the table.
	"idx_webhook_commands_webhook_id": "ON public.webhook_commands USING btree (webhook_id) WHERE (deleted_at IS NULL)",
	"idx_webhook_commands_command":    "ON public.webhook_commands USING btree (command) WHERE (deleted_at IS NULL)",
}

const boptGithubLoginLower = "ON public.users USING btree (lower(github_login))"

// Migration 606, on a database every migration has run on: up, down, up again,
// each in a transaction as sqlx runs it. The up leaves none of the dropped
// indexes and the lower(github_login) key the login lookup reads; the down
// brings each back as its own migration wrote it; webhook_commands goes only
// while it is empty, and a legacy reaction table's two indexes go with the rest
// on an install that kept one.
func TestBoptSmallIndexCleanupMigrationUpDownUp(t *testing.T) {
	ctx := context.Background()
	read := func(dir string) string {
		t.Helper()
		b, err := os.ReadFile("../../migrations/606_bopt_small_index_cleanup." + dir + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	var statements strings.Builder // the up without its -- comments, which may name CONCURRENTLY
	for _, line := range strings.Split(up, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		statements.WriteString(line + "\n")
	}
	if strings.Contains(strings.ToUpper(statements.String()), "CONCURRENTLY") || strings.HasPrefix(up, "-- no-transaction") {
		t.Fatal("606 is one transaction: a big table's drop goes in a file of its own")
	}
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	run := func(step, sql string) {
		t.Helper()
		tx, err := env.PG.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(sql); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s: %v", step, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	defs := func() map[string]string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = ANY($1)`,
			pq.Array(append(boptIndexNames(), "idx_users_github_login_lower")))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatal(err)
			}
			out[name] = def
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	commandsTable := func() bool {
		t.Helper()
		var name *string
		if err := env.PG.QueryRow(`SELECT to_regclass('webhook_commands')::text`).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name != nil
	}
	isUp := func(step string, commandsKept bool) {
		t.Helper()
		got := defs()
		for name, def := range got {
			if name != "idx_users_github_login_lower" && !(commandsKept && strings.HasPrefix(name, "idx_webhook_commands_")) {
				t.Errorf("%s: %s is still there: %s", step, name, def)
			}
		}
		if !strings.HasSuffix(got["idx_users_github_login_lower"], boptGithubLoginLower) {
			t.Errorf("%s: idx_users_github_login_lower = %q, want ...%s", step, got["idx_users_github_login_lower"], boptGithubLoginLower)
		}
		if commandsTable() != commandsKept {
			t.Errorf("%s: webhook_commands there = %v, want %v", step, !commandsKept, commandsKept)
		}
	}
	isDown := func(step string) {
		t.Helper()
		got := defs()
		for name, want := range boptSmallIndexes {
			if !strings.HasSuffix(got[name], " "+want) || !strings.Contains(got[name], " INDEX "+name+" ") {
				t.Errorf("%s: %s = %q, want ...%s", step, name, got[name], want)
			}
		}
		if def, ok := got["idx_users_github_login_lower"]; ok {
			t.Errorf("%s: the down left idx_users_github_login_lower: %s", step, def)
		}
	}

	// The harness ran every migration: 606 is up, and the empty registry gone.
	isUp("after the harness", false)

	// An install from before 52 whose reaction table held rows: 52 renamed it
	// to github_reaction_refs_legacy_38, with 38's two indexes on it.
	if _, err := env.PG.Exec(`CREATE TABLE github_reaction_refs_legacy_38 (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), task_id uuid NOT NULL, user_id uuid NOT NULL, github_name varchar NOT NULL,
		UNIQUE(task_id, user_id, github_name));
		CREATE INDEX idx_github_reaction_refs_task ON github_reaction_refs_legacy_38(task_id);
		CREATE INDEX idx_github_reaction_refs_lookup ON github_reaction_refs_legacy_38(task_id, user_id, github_name);`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.PG.Exec(`DROP TABLE IF EXISTS github_reaction_refs_legacy_38`) })
	run("up over a legacy table", up)
	isUp("up over a legacy table", false)

	run("down", down)
	isDown("down")
	if !commandsTable() {
		t.Fatal("down: webhook_commands is not back")
	}

	// A registry with a row in it stays through the up.
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'ci606', 'CI', NOW(), NOW())`,
		owner, owner.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	hook, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{Name: "Registry", Type: "incoming", BotName: "Registry"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO webhook_commands (webhook_id, command) VALUES ($1, '/deploy')`, hook.Id); err != nil {
		t.Fatal(err)
	}
	run("up over a registry with a row", up)
	isUp("up over a registry with a row", true)
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM webhook_commands`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the registry's row: %d %v", kept, err)
	}

	// Emptied, it goes on the next up; down and up again end where they began.
	if _, err := env.PG.Exec(`DELETE FROM webhook_commands`); err != nil {
		t.Fatal(err)
	}
	run("down again", down)
	isDown("down again")
	run("up again", up)
	isUp("up again", false)

	// The login lookup can read the lower() key: with scans off, its plan
	// names it (a key on github_login itself could not serve LOWER()).
	tx, err := env.PG.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	rows, err := tx.Query(`EXPLAIN SELECT id FROM users WHERE LOWER(github_login) = LOWER($1) AND deleted_at IS NULL`, "Octocat")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "idx_users_github_login_lower") {
		t.Errorf("the login lookup does not read idx_users_github_login_lower:\n%s", plan.String())
	}
}

func boptIndexNames() []string {
	names := make([]string, 0, len(boptSmallIndexes))
	for n := range boptSmallIndexes {
		names = append(names, n)
	}
	return names
}
