//go:build integration
// +build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// TestImportTokenRoundTrip exercises the SaveToken / LoadToken cycle
// against a real Postgres + the AES-256-GCM encryption layer the
// production deploy uses. It's the regression test for the OAuth
// refresh path: if encryption breaks, every provider stops being
// able to read its access token after the first request.
func TestImportTokenRoundTrip(t *testing.T) {
	t.Setenv("IMPORT_TOKEN_KEK", "integration-kek-1234567890abcdefghij")

	env := SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}

	// The integration harness applies every migration but doesn't seed
	// users. SaveToken FKs into users(id), so a minimal user row goes in:
	// id and email_id, the only columns without a default. (This used to
	// insert columns users doesn't have and skip itself, so it never ran.)
	ownerID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, ownerID, "ci-test-"+ownerID.String()[:8]+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	expiry := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
	in := &importModels.Token{
		Provider:     importModels.ProviderJira,
		OwnerUserId:  ownerID,
		AccessToken:  "access-secret-do-not-leak",
		RefreshToken: "refresh-secret-do-not-leak",
		ExpiresAt:    &expiry,
	}

	ctx := context.Background()
	if err := importModels.SaveToken(ctx, in); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	out, err := importModels.LoadToken(ctx, importModels.ProviderJira, ownerID)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if out == nil {
		t.Fatal("LoadToken returned nil after SaveToken")
	}
	if out.AccessToken != in.AccessToken {
		t.Fatalf("access token didn't round-trip; got %q want %q", out.AccessToken, in.AccessToken)
	}
	if out.RefreshToken != in.RefreshToken {
		t.Fatalf("refresh token didn't round-trip; got %q want %q", out.RefreshToken, in.RefreshToken)
	}
	if out.ExpiresAt == nil || !out.ExpiresAt.Equal(expiry) {
		t.Fatalf("expires_at didn't round-trip; got %v want %v", out.ExpiresAt, expiry)
	}

	// Verify the on-disk blob is NOT plaintext. If GCM ever silently
	// turned into a no-op (e.g. wrong KEK), this would catch it.
	var rawAccess []byte
	if err := env.PG.QueryRow(`
		SELECT access_token_enc FROM import_oauth_tokens
		 WHERE provider = $1 AND owner_user_id = $2
	`, importModels.ProviderJira, ownerID).Scan(&rawAccess); err != nil {
		t.Fatalf("read raw cipher: %v", err)
	}
	if string(rawAccess) == in.AccessToken {
		t.Fatal("access_token_enc on disk equals plaintext — encryption regressed")
	}

	// Re-saving overwrites. Confirm the model's UPSERT contract.
	in.AccessToken = "rotated"
	in.RefreshToken = "rotated-rt"
	if err := importModels.SaveToken(ctx, in); err != nil {
		t.Fatalf("SaveToken (rotate): %v", err)
	}
	out2, _ := importModels.LoadToken(ctx, importModels.ProviderJira, ownerID)
	if out2 == nil || out2.AccessToken != "rotated" {
		t.Fatalf("rotation didn't persist; got %+v", out2)
	}
}
