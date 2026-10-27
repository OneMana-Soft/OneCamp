//go:build integration

package integration_test

// Only a workspace admin, in the app, decides about an old import's access
// (business/Import recheck.go): re-checking it with their own connection,
// undoing a re-check or keeping its tasks to its projects' admins. A member,
// the demo's visitor among them, is refused by the admin chain, and an admin
// through a token by the routes themselves; neither reaches the import. An
// admin's call does, and is told when nothing waits for it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOnlyAnAdminInTheAppDecidesAboutAnOldImportsAccess' -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importController "github.com/akashc777/OneCamp/controllers/Import"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyAnAdminInTheAppDecidesAboutAnOldImportsAccess(t *testing.T) {
	t.Setenv("JWT_SECRET", "import-access-recheck-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"admin", "member"} {
		ids[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, ids[n], n+"@example.com", n, n); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": ids[n].String(), "user_name": n})
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`, ids["admin"]); err != nil {
		t.Fatalf("make the admin: %v", err)
	}
	uids := dg.Mutate(t, nodes)

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
	r.Post("/admin/import-access/{import_id}/recheck", importController.RecheckImportAccess)
	r.Post("/admin/import-access/{import_id}/undo", importController.UndoImportAccessRecheck)
	r.Post("/admin/import-access/{import_id}/contain", importController.ContainImportAccess)
	call := func(as, path string) int {
		t.Helper()
		token, err := helpers.SignSessionToken(ids[as].String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	admin := userModel.UserInfo{}
	admin.UserPostgresInfo.Id, admin.UserPostgresInfo.IsAdmin, admin.UserDgraphInfo.Uid = ids["admin"], true, uids["admin"]
	someImport := uuid.NewString()
	for action, h := range map[string]http.HandlerFunc{"recheck": importController.RecheckImportAccess,
		"undo": importController.UndoImportAccessRecheck, "contain": importController.ContainImportAccess} {
		path := "/admin/import-access/" + someImport + "/" + action
		if code := call("member", path); code != http.StatusForbidden {
			t.Errorf("a member's %s: %d, want 403", action, code)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("import_id", someImport)
		req := httptest.NewRequest(http.MethodPost, path, nil).WithContext(context.WithValue(
			helpers.WithActsThroughToken(context.WithValue(ctx, helpers.UserInfoContextKey, admin)), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("an admin's token asking for a %s: %d %s, want 403", action, rec.Code, rec.Body.String())
		}
		if code := call("admin", path); code != http.StatusConflict {
			t.Errorf("the admin's %s of an import with nothing waiting: %d, want 409", action, code)
		}
	}
}
