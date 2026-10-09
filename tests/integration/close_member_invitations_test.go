//go:build integration
// +build integration

package integration_test

// Signing up never touches an account that already exists, and migration
// 204 closes the invitations that were left live for such accounts.
//
// Sign-up through an invitation used to set a password on an existing account
// that had none (one that joined through Google or GitHub). Whoever held a
// live invitation link for a member's address could give that account a
// password and sign in as its owner.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSignUpNeverTouchesAnExistingAccount -v

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestSignUpNeverTouchesAnExistingAccount(t *testing.T) {
	lr := setupLandReady(t)
	id, _, err := userBusiness.CreateUserWithMethod(lr.ctx, "oauth.person@example.test", "OAuth Person", nil, userModels.AuthMethodGoogle, false)
	if err != nil {
		t.Fatal(err)
	}
	lr.invite("oauth.person@example.test", "tok-takeover")

	rec := postJSON(t, authController.EmailSignup, map[string]string{"token": "tok-takeover", "name": "Someone Else", "password": "a password of my choosing"})
	var answer struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if rec.Code != http.StatusConflict || answer.Msg != "You already have an account; sign in instead." {
		t.Errorf("signing up for an address with an account: %d %s", rec.Code, rec.Body.String())
	}
	var hash *string
	if err := lr.env.PG.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != nil && *hash != "" {
		t.Fatal("signing up set a password on someone else's account")
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), "Authorization") {
		t.Fatal("signing up for someone else's address signed them in")
	}
}

// Migration 204 on a workspace that has such invitations: the live ones for a
// live member's address are marked joined; expired ones, ones for a
// deactivated or imported (external) account, and ones with no account are
// left. It runs again harmlessly, and its down migration changes nothing.
func TestMigration204ClosesInvitationsToMembers(t *testing.T) {
	lr := setupLandReady(t)
	_, here, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(here), "..", "..", "migrations"), lr.env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(203); err != nil {
		t.Fatalf("down to 203: %v", err)
	}

	member := func(email string, deactivated, external bool) {
		t.Helper()
		if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, username, created_at, updated_at, deleted_at, is_external)
			VALUES ($1, $2, $3, NOW(), NOW(), CASE WHEN $4 THEN NOW() END, $5)`,
			uuid.New(), email, "u-"+uuid.NewString()[:8], deactivated, external); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
	}
	member("ana@example.test", false, false)
	member("Bo@Example.test", false, false)
	member("gone@example.test", true, false)
	member("ext@example.test", false, true)
	for _, inv := range []struct{ email, token, status, expires, made string }{
		{"ana@example.test", "m204-live", "sent", "NOW() + interval '3 days'", "NOW()"},
		{"bo@example.test", "m204-noexpiry", "pending", "NULL", "NOW()"},
		{"ana@example.test", "m204-expired", "sent", "NOW() - interval '1 day'", "NOW()"},
		{"ana@example.test", "m204-oldnoexpiry", "pending", "NULL", "NOW() - interval '8 days'"},
		{"gone@example.test", "m204-deactivated", "sent", "NOW() + interval '3 days'", "NOW()"},
		{"ext@example.test", "m204-external", "sent", "NOW() + interval '3 days'", "NOW()"},
		{"new@example.test", "m204-nobody", "sent", "NOW() + interval '3 days'", "NOW()"},
	} {
		if _, err := lr.env.PG.Exec(`INSERT INTO invitations (email, status, token, token_expires_at, created_at)
			VALUES ($1, $2, $3, `+inv.expires+`, `+inv.made+`)`, inv.email, inv.status, inv.token); err != nil {
			t.Fatalf("seed %s: %v", inv.token, err)
		}
	}
	want := map[string]string{
		"m204-live": "joined", "m204-noexpiry": "joined",
		"m204-expired": "sent", "m204-oldnoexpiry": "pending", "m204-deactivated": "sent", "m204-external": "sent", "m204-nobody": "sent",
	}
	check := func(when string) {
		t.Helper()
		for token, status := range want {
			var got string
			if err := lr.env.PG.QueryRow(`SELECT status FROM invitations WHERE token = $1`, token).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != status {
				t.Errorf("%s: %s is %q, want %q", when, token, got, status)
			}
		}
	}

	if err := m.Migrate(204); err != nil {
		t.Fatalf("migration 204: %v", err)
	}
	check("after 204")
	if err := m.Migrate(203); err != nil {
		t.Fatalf("204 down: %v", err)
	}
	check("after 204 down")
	if err := m.Migrate(204); err != nil {
		t.Fatalf("204 again: %v", err)
	}
	check("after 204 again")
}
