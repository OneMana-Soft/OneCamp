//go:build integration
// +build integration

package integration

import "testing"

// TestSmoke is a tiny canary verifying the test harness can spin up
// Postgres, apply every migration, and run a SELECT. If this fails the
// rest of the integration suite is meaningless, so it runs first
// alphabetically.
func TestSmoke(t *testing.T) {
	env := SetupEnv(t)

	var n int
	if err := env.PG.QueryRow(`SELECT 1`).Scan(&n); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1, got %d", n)
	}

	// Sanity: the schema_migrations table created by golang-migrate
	// should exist because applyMigrations ran.
	var exists bool
	err := env.PG.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'schema_migrations'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("check migrations table: %v", err)
	}
	if !exists {
		t.Fatal("schema_migrations table was not created — migrations did not run")
	}
}

// TestGitHubWebhookDeliveryStatus exercises the new claim/complete/fail
// flow added in migration 61. It's the regression test for the
// "fire-and-forget loses events" bug fix.
func TestGitHubWebhookDeliveryStatus(t *testing.T) {
	env := SetupEnv(t)
	db := env.PG

	// First claim should return 'processing' for a brand-new delivery.
	var status string
	err := db.QueryRow(`
		INSERT INTO github_webhook_deliveries
		    (delivery_id, event_type, status, attempts, received_at, updated_at)
		VALUES ('delivery-1', 'issues', 'processing', 1, NOW(), NOW())
		RETURNING status
	`).Scan(&status)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if status != "processing" {
		t.Fatalf("expected 'processing', got %q", status)
	}

	// A second claim of the SAME delivery (simulating GitHub's retry
	// after a failed first attempt) should re-claim and bump attempts.
	var attempts int
	err = db.QueryRow(`
		INSERT INTO github_webhook_deliveries
		    (delivery_id, event_type, status, attempts, received_at, updated_at)
		VALUES ('delivery-1', 'issues', 'processing', 1, NOW(), NOW())
		ON CONFLICT (delivery_id) DO UPDATE
		    SET status      = 'processing',
		        attempts    = github_webhook_deliveries.attempts + 1,
		        updated_at  = NOW()
		    WHERE github_webhook_deliveries.status <> 'completed'
		RETURNING attempts
	`).Scan(&attempts)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected attempts=2 after retry, got %d", attempts)
	}

	// After completion, the same insert pattern must NOT update.
	if _, err := db.Exec(`
		UPDATE github_webhook_deliveries
		   SET status = 'completed', processed_at = NOW(), updated_at = NOW()
		 WHERE delivery_id = 'delivery-1'
	`); err != nil {
		t.Fatalf("mark completed: %v", err)
	}

	// Re-claim attempt should be a no-op (no rows updated).
	var rowsScanned int
	err = db.QueryRow(`
		WITH upserted AS (
			INSERT INTO github_webhook_deliveries
			    (delivery_id, event_type, status, attempts, received_at, updated_at)
			VALUES ('delivery-1', 'issues', 'processing', 1, NOW(), NOW())
			ON CONFLICT (delivery_id) DO UPDATE
			    SET status      = 'processing',
			        attempts    = github_webhook_deliveries.attempts + 1,
			        updated_at  = NOW()
			    WHERE github_webhook_deliveries.status <> 'completed'
			RETURNING 1
		)
		SELECT COUNT(*) FROM upserted
	`).Scan(&rowsScanned)
	if err != nil {
		t.Fatalf("re-claim post-completion: %v", err)
	}
	if rowsScanned != 0 {
		t.Fatalf("expected zero rows updated for already-completed delivery, got %d", rowsScanned)
	}
}
