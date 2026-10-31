//go:build integration

package integration_test

// Only a workspace admin, in the app, reads the card of old imports' access
// re-check or keeps one as it is (business/Import accessAdmin.go). A member,
// the demo's visitor among them, is refused by the admin chain, and an admin
// through a token by the routes themselves. The card names each import's
// projects, archived ones too, and tasks by id only. Keep moves only an import
// waiting for an admin: once kept, being re-checked or contained, it is
// refused.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOnlyAnAdminInTheAppListsOrKeepsAnOldImportsAccess' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOnlyAnAdminInTheAppListsOrKeepsAnOldImportsAccess(t *testing.T) {
	t.Setenv("JWT_SECRET", "import-access-list-secret")
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
	for _, n := range []string{"admin", "member", "visitor"} {
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

	// Four old Jira imports: one waiting for an admin, over a live project
	// and an archived one; one being re-checked; one an admin contained; and
	// one waiting that only the refused token's keep names.
	launch, archived, gone := uuid.New(), uuid.New(), uuid.New()
	for name, p := range map[string]uuid.UUID{"Launch": launch, "Archive": archived} {
		if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, p, name, ids["admin"]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`UPDATE projects SET deleted_at = NOW() WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	job := func(ranAt time.Time) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(`INSERT INTO import_jobs (provider, source_workspace_name, source, triggered_by, started_at)
			VALUES ('jira', 'acme.atlassian.net', 'api', $1, $2) RETURNING id`, ids["admin"], ranAt).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	review := func(imp, project uuid.UUID, state, summary string) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO import_access_reviews (import_id, provider, project_uuid, gap, state, summary)
			VALUES ($1, 'jira', $2, 'jira_security', $3, $4::jsonb)`, imp, project, state, summary); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	asked, waiting, running, contained := job(now.Add(-4*time.Hour)), job(now.Add(-3*time.Hour)), job(now.Add(-2*time.Hour)), job(now.Add(-time.Hour))
	review(asked, launch, "needs_admin", `{"reason": "owner_gone"}`)
	review(waiting, launch, "needs_admin", `{"reason": "no_connection"}`)
	review(waiting, archived, "needs_admin", `{"reason": "no_connection", "gone_or_hidden": 1, "gone_or_hidden_tasks": ["`+gone.String()+`"],
		"cursor": {"entity": "task", "source": "ACME-7"}}`)
	review(running, launch, "rechecking", `{}`)
	review(contained, launch, "contained", `{"contained": 4}`)

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
	r.Get("/admin/import-access", importController.ListImportAccess)
	r.Post("/admin/import-access/{import_id}/keep", importController.KeepImportAccess)
	call := func(t *testing.T, as, method, path string) (int, string) {
		t.Helper()
		token, err := helpers.SignSessionToken(ids[as].String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	keep := func(imp uuid.UUID) string { return "/admin/import-access/" + imp.String() + "/keep" }
	states := func(imp uuid.UUID) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT state || ':' || COALESCE(decided_by::text, '') FROM import_access_reviews WHERE import_id = $1 ORDER BY project_uuid`, imp)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	refused := func(t *testing.T, who string) {
		if code, _ := call(t, who, http.MethodGet, "/admin/import-access"); code != http.StatusForbidden {
			t.Errorf("the %s reading the card: %d, want 403", who, code)
		}
		if code, _ := call(t, who, http.MethodPost, keep(waiting)); code != http.StatusForbidden {
			t.Errorf("the %s keeping an import: %d, want 403", who, code)
		}
	}
	refused(t, "member")
	t.Run("the demo's visitor", func(t *testing.T) {
		t.Setenv("DEMO_MODE", "true")
		t.Setenv("DEMO_USER_EMAIL", "visitor@example.com")
		if !helpers.IsDemoVisitor("visitor@example.com") {
			t.Fatal("the visitor isn't the demo's")
		}
		refused(t, "visitor")
	})
	admin := userModel.UserInfo{}
	admin.UserPostgresInfo.Id, admin.UserPostgresInfo.IsAdmin, admin.UserDgraphInfo.Uid = ids["admin"], true, uids["admin"]
	for path, h := range map[string]http.HandlerFunc{"/admin/import-access": importController.ListImportAccess, keep(asked): importController.KeepImportAccess} {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("import_id", asked.String())
		req := httptest.NewRequest(http.MethodPost, path, nil).WithContext(context.WithValue(
			helpers.WithActsThroughToken(context.WithValue(ctx, helpers.UserInfoContextKey, admin)), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("an admin's token on %s: %d %s, want 403", path, rec.Code, rec.Body.String())
		}
	}
	for _, imp := range []uuid.UUID{waiting, asked} {
		for _, s := range states(imp) {
			if s != "needs_admin:" {
				t.Errorf("refused calls moved an import: %v", states(imp))
			}
		}
	}

	code, body := call(t, "admin", http.MethodGet, "/admin/import-access")
	if code != http.StatusOK {
		t.Fatalf("the admin reading the card: %d %s", code, body)
	}
	var card struct {
		Imports []struct {
			ImportID string `json:"import_id"`
			Site     string `json:"site"`
			RanBy    string `json:"ran_by"`
			State    string `json:"state"`
			Reason   string `json:"reason"`
			Projects []struct {
				ProjectUUID       string   `json:"project_uuid"`
				Name              string   `json:"name"`
				Archived          bool     `json:"archived"`
				GoneOrHiddenTasks []string `json:"gone_or_hidden_tasks"`
			} `json:"projects"`
		} `json:"imports"`
	}
	if err := json.Unmarshal([]byte(body), &card); err != nil {
		t.Fatalf("the card: %v %s", err, body)
	}
	if len(card.Imports) != 4 || card.Imports[0].ImportID != contained.String() || card.Imports[2].ImportID != waiting.String() {
		t.Fatalf("the card's imports, newest first: %s", body)
	}
	w := card.Imports[2]
	if w.State != "needs_admin" || w.Reason != "no_connection" || w.Site != "acme.atlassian.net" || w.RanBy != "admin" || len(w.Projects) != 2 {
		t.Errorf("the waiting import: %+v", w)
	}
	for _, p := range w.Projects {
		if (p.ProjectUUID == archived.String()) != p.Archived {
			t.Errorf("%s listed as archived %v", p.Name, p.Archived)
		}
		if p.ProjectUUID == archived.String() && (len(p.GoneOrHiddenTasks) != 1 || p.GoneOrHiddenTasks[0] != gone.String()) {
			t.Errorf("the archived project's listed task: %v", p.GoneOrHiddenTasks)
		}
	}
	if strings.Contains(body, "ACME-7") || strings.Contains(body, "cursor") {
		t.Errorf("the card carries a pass's cursor: %s", body)
	}

	if code, body := call(t, "admin", http.MethodPost, keep(running)); code != http.StatusConflict {
		t.Errorf("keeping an import being re-checked: %d %s, want 409", code, body)
	}
	if code, body := call(t, "admin", http.MethodPost, keep(contained)); code != http.StatusConflict {
		t.Errorf("keeping a contained import: %d %s, want 409", code, body)
	}
	if code, body := call(t, "admin", http.MethodPost, keep(waiting)); code != http.StatusOK {
		t.Fatalf("the admin keeping the waiting import: %d %s", code, body)
	}
	for _, s := range states(waiting) {
		if s != "kept:"+ids["admin"].String() {
			t.Errorf("the kept import's reviews: %v", states(waiting))
		}
	}
	audited(t, env.PG, "import.access_kept", 1)
	// Keeping it settles it: its held-back comments are let into search now,
	// not at the next start's pass (releaseSettled), and each review keeps its
	// counts.
	waitFor(t, "the kept import's held-back comments to be let in", func() bool {
		var released bool
		if err := env.PG.QueryRow(`SELECT COALESCE(bool_and(summary ? 'comments_released'), false)
			FROM import_access_reviews WHERE import_id = $1`, waiting).Scan(&released); err != nil {
			t.Fatal(err)
		}
		return released
	})
	var hidden int
	if err := env.PG.QueryRow(`SELECT (summary->>'gone_or_hidden')::int FROM import_access_reviews
		WHERE import_id = $1 AND project_uuid = $2 AND jsonb_typeof(summary) = 'object'`, waiting, archived).Scan(&hidden); err != nil || hidden != 1 {
		t.Errorf("the kept review's counts: gone_or_hidden %d (%v), want 1 in an object", hidden, err)
	}
	if code, body := call(t, "admin", http.MethodPost, keep(waiting)); code != http.StatusConflict {
		t.Errorf("keeping it again: %d %s, want 409", code, body)
	}
	if got := states(running); len(got) != 1 || got[0] != "rechecking:" {
		t.Errorf("the import being re-checked: %v", got)
	}
}
