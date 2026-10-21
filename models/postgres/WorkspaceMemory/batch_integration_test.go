//go:build integration

package models_test

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	memory "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Backend audit U2b: forgetting an agent's instructions dismissed each item
// alone, and the nudge pass detached each stale owner alone. Each is one
// statement, and does to every item what it did to one.
func TestMemoryItemsAreDismissedAndDetachedTogether(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'o@example.test')`, owner); err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for i, c := range []string{"Ship the launch", "Write the notes", "Book the room"} {
		id, err := memory.Upsert(ctx, memory.UpsertInput{Kind: memory.KindCommitment, Content: c, Status: "open", OwnerID: &owner,
			DedupHash: memory.DedupHash(memory.KindCommitment, c, "", "", string(rune('a'+i))), Confidence: 80})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := memory.ClearOwners(ctx, ids[:2]); err != nil {
		t.Fatal(err)
	}
	var owned int
	if err := env.PG.QueryRow(`SELECT count(*) FROM workspace_memory_items WHERE owner_user_id IS NOT NULL`).Scan(&owned); err != nil || owned != 1 {
		t.Errorf("owned after detaching two of three: %d %v", owned, err)
	}
	gone, err := memory.SoftDeleteMany(ctx, append(ids[1:], ids[1]))
	if err != nil || len(gone) != 2 {
		t.Fatalf("dismissed: %v %v", gone, err)
	}
	again, err := memory.SoftDeleteMany(ctx, ids)
	if err != nil || len(again) != 1 || again[0] != ids[0] {
		t.Errorf("a second dismissal took %v (%v); only the one left", again, err)
	}
}
