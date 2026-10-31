//go:build integration
// +build integration

package integration_test

// Two admins who change an app's access from what each of them read can't
// both win (CR-15's SF1 on app access, 22:54 on 23 Oct 2026): the identity an
// admin reads carries its access version, a change is sent with it in
// If-Match, and one sent from a version someone already changed is refused
// with 412 and changes nothing. Against real Postgres, Dgraph and Redis,
// through the router an admin's screen calls.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAppsAccessChangesOnlyFromTheVersionItWasRead -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnAppsAccessChangesOnlyFromTheVersionItWasRead(t *testing.T) {
	t.Setenv("JWT_SECRET", "app-access-version-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	// The version is what is under test; the CSRF check has its own tests.
	t.Setenv("CSRF_DISABLED", "1")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)

	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "versions-admin@example.test", "Versions Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "versions-admin@example.test"); err != nil {
		t.Fatal(err)
	}
	app, err := commandBusiness.CreateApp(ctx, commandAdapter.CreateAppRequest{Slug: "release-bot", Name: "Release bot", Kind: "external"}, adminID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := helpers.SignSessionToken(adminID.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}

	routes := router.Routes()
	// call answers an admin's request; it is safe from several goroutines.
	call := func(method, path, ifMatch string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: session})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	accessPath := "/admin/apps/" + app.ID + "/access"
	identity := func() map[string]any {
		t.Helper()
		code, out := call(http.MethodGet, "/admin/apps/"+app.ID+"/identity", "", nil)
		data, _ := out["data"].(map[string]any)
		identity, _ := data["identity"].(map[string]any)
		if code != http.StatusOK || identity == nil {
			t.Fatalf("reading the app answered %d %v", code, out)
		}
		return identity
	}
	versionOf := func(identity map[string]any) string {
		v, _ := identity["access_version"].(string)
		return v
	}
	access := func(scopes ...string) map[string]any {
		return map[string]any{"scopes": scopes, "audience": map[string]any{"places": []any{}, "public": false}}
	}
	quoted := func(version string) string { return `"` + version + `"` }

	read := versionOf(identity())
	if read == "" {
		t.Fatal("the app's identity carries no access_version to change it from")
	}

	t.Run("a change from the version read is taken, and moves the version", func(t *testing.T) {
		code, out := call(http.MethodPut, accessPath, quoted(read), access("tables:read"))
		data, _ := out["data"].(map[string]any)
		if code != http.StatusOK || data == nil {
			t.Fatalf("the change answered %d %v", code, out)
		}
		if now := versionOf(identity()); versionOf(data) == "" || versionOf(data) == read || versionOf(data) != now {
			t.Fatalf("after the change it answered version %q, it was %q and the app says %q", versionOf(data), read, now)
		}
	})

	t.Run("a change from a version someone already changed is refused, and changes nothing", func(t *testing.T) {
		before := identity()
		code, out := call(http.MethodPut, accessPath, quoted(read), access("tasks:read"))
		if code != http.StatusPreconditionFailed || out["msg"] != "Someone changed what this app may do since you opened it." {
			t.Fatalf("a change from a stale version answered %d %v, want 412 and its words", code, out)
		}
		after := identity()
		if versionOf(after) != versionOf(before) || fmt.Sprint(after["scopes"]) != fmt.Sprint(before["scopes"]) {
			t.Fatalf("the refused change moved the app from %v (%s) to %v (%s)", before["scopes"], versionOf(before), after["scopes"], versionOf(after))
		}
	})

	t.Run("two changes sent from one version: one is taken, the other refused", func(t *testing.T) {
		from := quoted(versionOf(identity()))
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, scopes := range [][]string{{"tasks:read"}, {"tasks:read", "tables:read"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i], _ = call(http.MethodPut, accessPath, from, access(scopes...))
			}()
		}
		wg.Wait()
		sort.Ints(codes)
		if codes[0] != http.StatusOK || codes[1] != http.StatusPreconditionFailed {
			t.Fatalf("two changes from one version answered %v, want one 200 and one 412", codes)
		}
	})

	t.Run("a change sent with no version, or any, is taken as before", func(t *testing.T) {
		for _, ifMatch := range []string{"", "*"} {
			if code, out := call(http.MethodPut, accessPath, ifMatch, access("tables:read")); code != http.StatusOK {
				t.Fatalf("a change with If-Match %q answered %d %v", ifMatch, code, out)
			}
		}
	})
}
