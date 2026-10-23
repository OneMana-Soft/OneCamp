//go:build integration
// +build integration

package integration_test

// Links an older server left in the clear (audit A1), against real Postgres
// 12: a table that can't be hashed at the start doesn't keep the others in
// the clear, the start tries again until it can, and until then the admin's
// system check says links are still kept in the clear.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestLinksLeftInClearAreHashedPastAFailingTableAndShownUntilThen$' -v

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	linkSecretModel "github.com/akashc777/OneCamp/models/postgres/LinkSecret"
)

// linksCheck is the admin's system check about links kept in the clear.
func linksCheck(t *testing.T, ctx context.Context) helpers.SystemCheckResult {
	t.Helper()
	for _, r := range helpers.RunSystemChecks(ctx, 10*time.Second) {
		if r.Name == "links" {
			return r
		}
	}
	t.Fatal("no system check is named links")
	return helpers.SystemCheckResult{}
}

func TestLinksLeftInClearAreHashedPastAFailingTableAndShownUntilThen(t *testing.T) {
	lr := setupLandReady(t)
	ctx, cancel := context.WithCancel(lr.ctx)
	t.Cleanup(cancel)
	// An invitation the old server sent after the migration, with its link in
	// the clear and no hash.
	leftInClear := func(email string) string {
		t.Helper()
		token := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
		if _, err := lr.env.PG.Exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at)
			VALUES ($1, $2, 'sent', $3, NOW() + interval '7 days')`, email, lr.owner.UserPostgresInfo.Id, token); err != nil {
			t.Fatal(err)
		}
		return token
	}
	// locked holds table locked from another connection until the returned
	// func lets it go, so every statement on it waits out its timeout.
	locked := func(table string) (release func()) {
		t.Helper()
		tx, err := lr.env.PG.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`LOCK TABLE ` + table + ` IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		released := false
		release = func() {
			if !released {
				released = true
				_ = tx.Rollback()
			}
		}
		t.Cleanup(release)
		return release
	}

	// Bookings can't be hashed (another session holds them): invitations,
	// after them, still are.
	kim := leftInClear("kim@example.test")
	releaseBookings := locked("bookings")
	err := linkSecretModel.HashLeftInClear(ctx)
	if err == nil || !strings.Contains(err.Error(), "bookings") {
		t.Errorf("with bookings locked the hashing answers %v", err)
	}
	releaseBookings()
	if n := invitationsHolding(t, lr, kim); n != 0 || !invitationLinkTaken(kim) {
		t.Errorf("past locked bookings: %d invitations keep Kim's link in the clear, and it's taken: %v", n, invitationLinkTaken(kim))
	}
	if r := linksCheck(t, ctx); !r.Healthy {
		t.Errorf("with every link hashed the check fails: %s", r.Detail)
	}

	// At the start, invitations can't be hashed: the server starts anyway,
	// and hashes them once it can.
	lee := leftInClear("lee@example.test")
	if r := linksCheck(t, ctx); r.Healthy || !strings.Contains(r.Detail, "invitations") {
		t.Errorf("with an invitation's link in the clear the check says: healthy %v, %q", r.Healthy, r.Detail)
	}
	releaseInvitations := locked("invitations")
	restore := linkSecretModel.SetStartRetriesForTest(helpers.Retries{First: 50 * time.Millisecond, Cap: 200 * time.Millisecond, Tries: 100})
	t.Cleanup(restore)
	linkSecretModel.HashLeftInClearAtStart(ctx)
	// Its first try waited out its timeout on the lock, so only a retry can
	// hash Lee's link now.
	releaseInvitations()
	deadline := time.Now().Add(30 * time.Second)
	for invitationsHolding(t, lr, lee) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Lee's link is still in the clear 30 s after invitations were let go")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !invitationLinkTaken(lee) {
		t.Error("Lee's link isn't taken once it's hashed")
	}
	if r := linksCheck(t, ctx); !r.Healthy {
		t.Errorf("once the start's retry hashed Lee's link the check fails: %s", r.Detail)
	}
}
