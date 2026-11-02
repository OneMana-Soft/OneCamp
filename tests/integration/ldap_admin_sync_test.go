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
	userDomain "github.com/akashc777/OneCamp/domain/User"
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
	frank := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, signup_method, is_sso_managed, created_at, updated_at)
		VALUES ($1, 'frank@example.test', 'frank', 'ldap', true, NOW(), NOW())`, frank); err != nil {
		t.Fatal(err)
	}
	// Whether frank is an admin, as the sign-in itself reads it.
	isAdmin := func() bool {
		t.Helper()
		yes, _, err := userDomain.GetUserSignInFlags(ctx, frank)
		if err != nil {
			t.Fatal(err)
		}
		return yes
	}
	// frank's admin rows: all of them, and those taken away by nobody (the
	// directory, not a person, takes a role at sign-in).
	adminRows := func(where string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_users WHERE email_id = 'frank@example.test'` + where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
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
	// Each change the directory makes is in the admin audit log (audit C2b),
	// as the workspace acting on its own.
	if kinds := audited(t, env.PG, "admin.granted", 1); kinds[0] != "system" {
		t.Errorf("the grant was written as %q, want the system", kinds[0])
	}
	groups = nil
	signIn()
	if isAdmin() {
		t.Fatal("taken out of the directory's admin group, and still an admin")
	}
	audited(t, env.PG, "admin.revoked", 1)
	// The role is kept, taken by nobody, with when (V-184). The workspace has
	// had an admin, so its first-admin setup stays closed: it is one-time
	// (docs/customer-onboarding.md), not open to anyone once the last admin
	// role goes.
	if n := adminRows(""); n != 1 {
		t.Fatalf("the admin role the directory took from frank is gone (%d rows), and with it who had it", n)
	}
	if n := adminRows(" AND deleted_at IS NOT NULL AND deleted_by IS NULL"); n != 1 {
		t.Error("the directory's take-away is not marked, or is marked by someone")
	}
	if exists, err := userDomain.AdminSetupDone(ctx); err != nil || !exists {
		t.Errorf("after the directory took the only admin role: the workspace has had an admin = %v (%v), want true", exists, err)
	}

	// Back in the group: the same row comes back, live.
	groups = []string{adminGroup}
	signIn()
	if !isAdmin() {
		t.Fatal("back in the directory's admin group: an admin again")
	}
	if n := adminRows(" AND deleted_at IS NULL AND deleted_by IS NULL"); n != 1 || adminRows("") != 1 {
		t.Errorf("back in the group: %d live rows of %d, want its one row back, live", n, adminRows(""))
	}
	audited(t, env.PG, "admin.granted", 2)
}
