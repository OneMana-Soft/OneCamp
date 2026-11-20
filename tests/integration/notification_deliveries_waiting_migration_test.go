//go:build integration
// +build integration

package integration_test

// Migration 592 (A4 H4) goes up, down and up again on Postgres 12: up lets a
// push row wait ('waiting') and indexes the waiting rows by when they're due;
// down drops what was still waiting (never sent) and puts 585's states back.
//
// Run: go test -tags=integration ./tests/integration/ -run TestNotificationDeliveriesWaitingMigrationUpDownUp -v

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNotificationDeliveriesWaitingMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	read := func(direction string) string {
		b, err := os.ReadFile(filepath.Join(dir, "592_notification_deliveries_waiting."+direction+".sql"))
		if err != nil {
			t.Fatalf("migration 592 %s: %v", direction, err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")

	env := integration.SetupEnv(t)
	user := uuid.New()
	wait := func(event string) error {
		_, err := env.PG.Exec(`INSERT INTO notification_deliveries (event_id, user_id, delivery, cause, state, hold_until)
			VALUES ($1, $2, 'push', 'chat.mention', 'waiting', NOW() + interval '1 minute')`, event, user)
		return err
	}
	indexed := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'notification_deliveries' AND indexname = 'notification_deliveries_waiting_idx'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	state := func(event string) string {
		var s string
		if err := env.PG.QueryRow(`SELECT state FROM notification_deliveries WHERE event_id = $1`, event).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := wait("e1"); err != nil || !indexed() {
		t.Fatalf("up: a waiting row %v, indexed %v", err, indexed())
	}
	for round := 0; round < 2; round++ {
		if _, err := env.PG.Exec(down); err != nil {
			t.Fatalf("down %d: %v", round, err)
		}
		if got := state("e1"); got != "dropped" {
			t.Errorf("down %d: the waiting row is %q, want dropped", round, got)
		}
		if err := wait("e2"); err == nil || indexed() {
			t.Fatalf("down %d: a waiting row was kept (%v) or the index stays (%v)", round, err, indexed())
		}
		if _, err := env.PG.Exec(up); err != nil {
			t.Fatalf("up %d: %v", round, err)
		}
		if _, err := env.PG.Exec(`UPDATE notification_deliveries SET state = 'waiting' WHERE event_id = 'e1'`); err != nil || !indexed() {
			t.Fatalf("up %d: waiting again %v, indexed %v", round, err, indexed())
		}
	}
}
