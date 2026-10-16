//go:build integration
// +build integration

package integration

// Does taking someone's admin role away take it away on their next request?
//
// Whether a person is an admin travels with their account row, which the auth
// middleware reads through a cache kept for an hour on every authenticated
// request. Every change to it forgets that cache through one function
// (domain/User forgetProfile), and this executes the sequence that matters:
// an admin's request warms the cache, the role goes, and the very next request
// is refused. It also covers the read that was already in flight when the role
// went, which writes the old row back after the first forget.
//
// THE TEST MUST HAVE A REAL CACHE OR IT PROVES NOTHING: without Redis every read
// misses and goes to the database, and the test passes against code that never
// forgets anything. Hence SetupRedis, and the check that the cache was warm.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

func TestDemotingAnAdminRefusesTheirNextRequest(t *testing.T) {
	t.Setenv("JWT_SECRET", "admin-demotion-integration-secret")
	env := SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	SetupRedis(t)
	StubMqttClient(t)
	dg := SetupDgraph(t)

	adminID := uuid.New()
	id, email := adminID.String(), "admin-"+adminID.String()[:8]+"@example.com"
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, adminID, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, email); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	dg.Mutate(t, []map[string]any{{"uid": "_:admin", "user_uuid": id, "user_name": "ada", "user_full_name": "Ada"}})

	// The route an admin page calls, behind the middleware the router puts there.
	adminOnly := customMiddleware.VerifyAuth(customMiddleware.VerifyAdminAuthOnlyPostgres(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	request := func() int {
		t.Helper()
		token, err := helpers.SignSessionToken(id, helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		adminOnly.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := request(); code != http.StatusOK {
		t.Fatalf("an admin's request to an admin route: HTTP %d, want 200", code)
	}
	var asAdmin userModels.User
	if found, _ := redisStore.GetJSON(ctx, registry.UserProfile, []string{id}, &asAdmin); !found || !asAdmin.IsAdmin {
		t.Fatal("the request did not leave an admin's profile in the cache, so nothing below tests the cache")
	}

	t.Run("demoted, their next request is refused", func(t *testing.T) {
		if err := userBusiness.HardDeleteAdminUserByEmailId(ctx, email, id); err != nil {
			t.Fatal(err)
		}
		if code := request(); code != http.StatusForbidden {
			t.Fatalf("the request right after the demotion: HTTP %d, want 403", code)
		}
	})

	t.Run("made an admin again, their next request is an admin's", func(t *testing.T) {
		if err := userBusiness.CreateAdminUser(ctx, email, id); err != nil {
			t.Fatal(err)
		}
		if code := request(); code != http.StatusOK {
			t.Fatalf("the request right after the promotion: HTTP %d, want 200", code)
		}
	})

	t.Run("a read in flight during the demotion can't keep the old role for long", func(t *testing.T) {
		if err := userBusiness.HardDeleteAdminUserByEmailId(ctx, email, id); err != nil {
			t.Fatal(err)
		}
		// What a request that read the row just before the demotion writes just
		// after the first forget: the admin's row. For a moment it is served.
		if err := redisStore.SetJSON(ctx, registry.UserProfile, []string{id}, asAdmin); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(6 * time.Second)
		for request() != http.StatusForbidden {
			if time.Now().After(deadline) {
				t.Fatal("the stale admin row was still served 6 seconds after the demotion")
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	t.Run("deactivated, their next request is refused", func(t *testing.T) {
		if err := userBusiness.CreateAdminUser(ctx, email, id); err != nil {
			t.Fatal(err)
		}
		if code := request(); code != http.StatusOK {
			t.Fatalf("an admin again: HTTP %d, want 200", code)
		}
		if err := userBusiness.DeactivateUser(ctx, adminID); err != nil {
			t.Fatal(err)
		}
		if code := request(); code != http.StatusUnauthorized {
			t.Fatalf("the request right after the deactivation: HTTP %d, want 401", code)
		}
	})
}
