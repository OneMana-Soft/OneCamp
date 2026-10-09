//go:build integration
// +build integration

package integration_test

// The directory's admin groups decide who is an admin, both ways.
//
// A directory sign-in promotes someone in one of the configured admin groups,
// and should demote a directory-managed admin who no longer is. The demotion
// never happened: the sign-in read the person without their admin flag, so
// it always looked like they weren't one. Signed in through LDAPLogin with the
// directory stood in for, everything after its answer real.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDirectoryAdminFollowsItsGroups -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authController "github.com/akashc777/OneCamp/controllers/Auth"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestDirectoryAdminFollowsItsGroups(t *testing.T) {
	// As Active Directory gives a group: its DN. Configured by its name.
	const adminGroup = "CN=OneCamp Admins,OU=Groups,DC=example,DC=test"
	t.Setenv("JWT_SECRET", "ldap-admin-sync-integration-secret")
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "directory.example.test")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=test")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	t.Setenv("LDAP_ADMIN_GROUPS", "OneCamp Admins")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = ""

	var groups []string // what the directory says frank is in, each sign-in
	t.Cleanup(authController.UseDirectoryForTest(func(_ *ldapService.LDAPClient, who, password string) (*ldapService.LDAPUser, error) {
		return &ldapService.LDAPUser{DN: "uid=" + who + ",dc=example,dc=test", Email: who + "@example.test", Username: who, Groups: groups}, nil
	}))
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, signup_method, is_sso_managed, created_at, updated_at)
		VALUES ($1, 'frank@example.test', 'frank', 'ldap', true, NOW(), NOW())`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	isAdmin := func() bool {
		t.Helper()
		var yes bool
		if err := env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM admin_users WHERE email_id = 'frank@example.test')`).Scan(&yes); err != nil {
			t.Fatal(err)
		}
		return yes
	}
	signIn := func() {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"username_or_email": "frank", "password": "pw"})
		rec := httptest.NewRecorder()
		authController.LDAPLogin(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
		}
	}

	groups = []string{adminGroup}
	signIn()
	if !isAdmin() {
		t.Fatal("in the directory's admin group: an admin")
	}
	groups = nil
	signIn()
	if isAdmin() {
		t.Fatal("taken out of the directory's admin group, and still an admin")
	}
}
