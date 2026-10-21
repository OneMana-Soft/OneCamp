//go:build integration
// +build integration

package integration_test

// The admin's list of imports (GET /admin/import/jobs) asks Postgres the same
// few times however many imports it shows, and turns its pages (backend
// audit U1b): it read five statements an import (its chunks, items, errors,
// status and priority mappings), and ?offset was read and never used, so
// every page was the first.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheImportsListIsCountedOnceAndPaged -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	importController "github.com/akashc777/OneCamp/controllers/Import"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheImportsListIsCountedOnceAndPaged(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'admin@acme.test')`, admin); err != nil {
		t.Fatal(err)
	}
	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, EmailID: "admin@acme.test", IsAdmin: true},
	})
	var ids []uuid.UUID
	addJobs := func(n int) {
		for i := 0; i < n; i++ {
			id := uuid.New()
			ids = append(ids, id)
			if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, created_at)
				VALUES ($1, 'asana', $2, 'api', 'completed', NOW() - make_interval(mins => $3))`, id, fmt.Sprintf("W%02d", len(ids)), len(ids)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.PG.Exec(`INSERT INTO import_status_mappings (import_id, source_status, target_status) VALUES ($1, 'complete', 'done')`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	list := func(query string) []map[string]any {
		rec := httptest.NewRecorder()
		importController.HandleListJobs(rec, httptest.NewRequest(http.MethodGet, "/admin/import/jobs?"+query, nil).WithContext(asAdmin))
		if rec.Code != http.StatusOK {
			t.Fatalf("list %q: %d %s", query, rec.Code, rec.Body)
		}
		var body struct {
			Jobs []map[string]any `json:"jobs"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Jobs
	}

	counter := integration.CountPostgres(t, env)
	addJobs(3)
	counter.Reset()
	if got := list("limit=50"); len(got) != 3 {
		t.Fatalf("3 imports listed as %d", len(got))
	}
	few := counter.Count()
	addJobs(27)
	counter.Reset()
	got := list("limit=50")
	many := counter.Count()
	if len(got) != 30 {
		t.Fatalf("30 imports listed as %d", len(got))
	}
	if many != few {
		t.Errorf("the list asked Postgres %d times for 3 imports and %d for 30", few, many)
	}
	if m, _ := got[0]["status_mappings"].(map[string]any); m["complete"] != "done" {
		t.Errorf("an import's status mappings: %v", got[0]["status_mappings"])
	}

	// Newest first: the second page of ten starts with the eleventh.
	page := list("limit=10&offset=10")
	if len(page) != 10 || page[0]["source_workspace_name"] != "W11" {
		var first any
		if len(page) > 0 {
			first = page[0]["source_workspace_name"]
		}
		t.Errorf("the second page: %d imports from %v, want 10 from W11", len(page), first)
	}
}
