//go:build integration

package domain

// Read receipts' storage against Postgres 12 with every migration: a mark
// only moves forward, a conversation's marks read back together, a new
// conversation's others start at nothing, and the read_receipts preference
// defaults on and turns off.
// Run: go test -tags=integration ./domain/LastSeenChat/ -v

import (
	"context"
	"testing"
	"time"

	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestReadReceiptStorage(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	me, maya, jonas := uuid.New(), uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{me, maya, jonas} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, []string{"me", "maya", "jonas"}[i]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	grp := "grp-" + uuid.NewString()[:8]
	later := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)

	// A new conversation: the others start at nothing, and a second "start"
	// doesn't touch a mark that's there.
	if err := CreateOrUpdateLastSeenChat(ctx, maya, grp, later); err != nil {
		t.Fatal(err)
	}
	if err := BulkCreateLastSeenChatIfNotExists(ctx, []string{maya.String(), jonas.String()}, grp); err != nil {
		t.Fatal(err)
	}
	seen, err := GetLastSeenForGrouping(ctx, grp)
	if err != nil {
		t.Fatal(err)
	}
	if !seen[maya.String()].Equal(later) || seen[jonas.String()].Year() != 1970 || len(seen) != 2 {
		t.Fatalf("Maya's mark stays, Jonas starts at nothing: %v", seen)
	}

	// A mark only moves forward, one at a time or in bulk.
	if err := CreateOrUpdateLastSeenChat(ctx, maya, grp, earlier); err != nil {
		t.Fatal(err)
	}
	if err := BulkCreateOrUpdateLastSeenChat(ctx, []string{maya.String()}, grp, earlier); err != nil {
		t.Fatal(err)
	}
	if err := BulkUpdateLastSeenChatForSender(ctx, []string{grp}, maya.String(), earlier); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := GetLastSeenChat(ctx, maya, grp); !got.Equal(later) {
		t.Fatalf("an earlier time took Maya's mark back to %v", got)
	}
	if err := CreateOrUpdateLastSeenChat(ctx, jonas, grp, later); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := GetLastSeenChat(ctx, jonas, grp); !got.Equal(later) {
		t.Fatalf("a later time moves Jonas's mark: %v", got)
	}

	// read_receipts is on until someone turns it off.
	if _, err := prefDomain.LoadOrCreate(ctx, me); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := prefDomain.LoadOrCreate(ctx, maya); err != nil {
		t.Fatal(err)
	}
	if err := prefModels.Update(maya, prefModels.UpdatePreferenceInput{ReadReceipts: &off}); err != nil {
		t.Fatal(err)
	}
	prefs, err := prefDomain.LoadByUserIDs(ctx, []uuid.UUID{me, maya, jonas})
	if err != nil {
		t.Fatal(err)
	}
	if prefs[me] == nil || !prefs[me].ReadReceipts || prefs[maya] == nil || prefs[maya].ReadReceipts || prefs[jonas] != nil {
		t.Fatalf("me on by default, Maya off, Jonas with no row yet: %+v %+v %+v", prefs[me], prefs[maya], prefs[jonas])
	}
}
