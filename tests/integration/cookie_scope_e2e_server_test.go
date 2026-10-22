//go:build integration
// +build integration

package integration_test

// One workspace's API for the COOKIE-SCOPE e2e (web repo,
// e2e/cookieScope/run.sh): the real router against real Postgres, Redis and
// Dgraph, served on a local port, with one member who signs in with a
// password. The e2e runs two of these, each with its own key and database, as
// two Cloud workspaces on one zone (acme.ws.test, beta.ws.test), and opens both
// in one browser.
//
// Not a test of its own: it skips unless the e2e runner sets
// COOKIE_SCOPE_E2E_ADDR, and then serves until COOKIE_SCOPE_E2E_STOP exists.
//
// Run by e2e/cookieScope/run.sh only.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCookieScopeWorkspaceServer(t *testing.T) {
	addr := os.Getenv("COOKIE_SCOPE_E2E_ADDR")
	if addr == "" {
		t.Skip("serves one workspace for the web repo's e2e/cookieScope/run.sh, which sets COOKIE_SCOPE_E2E_ADDR")
	}
	ready, stop := os.Getenv("COOKIE_SCOPE_E2E_READY"), os.Getenv("COOKIE_SCOPE_E2E_STOP")
	email, password := os.Getenv("COOKIE_SCOPE_E2E_EMAIL"), os.Getenv("COOKIE_SCOPE_E2E_PASSWORD")
	for name, v := range map[string]string{"COOKIE_SCOPE_E2E_READY": ready, "COOKIE_SCOPE_E2E_STOP": stop, "COOKIE_SCOPE_E2E_EMAIL": email, "COOKIE_SCOPE_E2E_PASSWORD": password} {
		if v == "" {
			t.Fatalf("%s is not set", name)
		}
	}
	for _, name := range []string{"JWT_SECRET", "FE_DOMAIN", "FE_HOST_DOMAIN", "BACKEND_DOMAIN"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			t.Fatalf("%s is not set: the runner configures each workspace as Cloud does", name)
		}
	}

	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hashed := string(hash)
	if _, _, err := userBusiness.CreateUserWithMethod(ctx, email, "Sam Rivera", &hashed, userModels.AuthMethodEmail, false); err != nil {
		t.Fatalf("create the member: %v", err)
	}
	// The workspace's admin, so the sign-in page doesn't send to first-run setup.
	if err := userBusiness.CreateAdminUser(ctx, email); err != nil {
		t.Fatalf("make the member the admin: %v", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: router.Routes(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	if err := os.WriteFile(ready, []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("serving %s on %s", os.Getenv("BACKEND_DOMAIN"), addr)

	deadline := time.Now().Add(45 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			break
		}
		select {
		case err := <-served:
			t.Fatalf("the server stopped: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Logf("shutdown: %v", err)
	}
}
