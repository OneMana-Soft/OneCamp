//go:build integration
// +build integration

package integration_test

// Installing one of OneCamp's table templates is New table's permission
// (gsheets-structure-05): any member may, and the session check refuses
// everyone who is not a member (external people, bots) before anything is
// made. A guard: it passes on the code before the templates too.
// Run: go test -tags=integration ./tests/integration/ -run TestTableTemplateInstall -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	marketplaceController "github.com/akashc777/OneCamp/controllers/Marketplace"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTableTemplateInstallIsForMembersOnly(t *testing.T) {
	t.Setenv("JWT_SECRET", "table-template-install-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	seed := func(email, name string, external bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_external, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`, id, email, name, name, external); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
		node := map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(),
			"user_email_id": email, "user_name": name, "user_full_name": name}
		if external {
			node["is_external"] = true
		}
		dg.Mutate(t, node)
		return id
	}
	member := seed("mia@example.com", "mia", false)
	outsider := seed("olly@example.org", "jira-olly", true)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(customMiddleware.VerifyAuth)
		r.Post("/marketplace/templates/{id}/install", marketplaceController.InstallTemplate)
	})
	install := func(who uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		token, err := helpers.SignSessionToken(who.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/marketplace/templates/b5d0c000-0000-4000-8000-000000000101/install", nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	tables := func(who uuid.UUID) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM data_tables WHERE created_by = $1`, who).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if rec := install(outsider); rec.Code != http.StatusUnauthorized {
		t.Errorf("an external person's install answered %d, want 401", rec.Code)
	}
	if n := tables(outsider); n != 0 {
		t.Errorf("an external person's install made %d tables", n)
	}

	// The fixture holds: a member's session on the same route installs.
	rec := install(member)
	var body struct {
		Data struct {
			Kind     string `json:"kind"`
			EntityID string `json:"entity_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body.Data.Kind != "table" || body.Data.EntityID == "" {
		t.Fatalf("the fixture is wrong: a member's install answered %d %s", rec.Code, rec.Body.String())
	}
	if n := tables(member); n != 3 {
		t.Errorf("a member's CRM install made %d tables, want 3", n)
	}
}
