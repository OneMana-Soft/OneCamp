//go:build integration
// +build integration

package integration_test

// The review records are written however many an install's old imports made.
// One INSERT binds four values a review, and Postgres takes at most 65,535
// binds in a statement, so the writes go in batches: 16,500 reviews (66,000
// binds) land whole, and writing them again adds nothing. A project's people
// are read a page at a time, each once. Only imports first run before the
// importers' fix are candidates.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestImportReviewsAreWrittenInBatches|TestOnlyImportsFirstRunBeforeTheFixAreCandidates' -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestImportReviewsAreWrittenInBatches(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	ids := func(q string) []uuid.UUID {
		t.Helper()
		rows, err := env.PG.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	people := ids(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		SELECT uuid_generate_v4(), 'person' || g || '@example.com', 'person' || g, 'Person ' || g, NOW(), NOW()
		FROM generate_series(1, 150) g RETURNING id`)
	projects := ids(`INSERT INTO projects (id, project_name, created_by)
		SELECT uuid_generate_v4(), 'Project ' || g, (SELECT id FROM users ORDER BY username LIMIT 1)
		FROM generate_series(1, 110) g RETURNING id`)
	job := ids(`INSERT INTO import_jobs (provider, source_workspace_name, source) VALUES ('todoist', 'Old workspace', 'api') RETURNING id`)[0]

	var cands []reviewModel.Candidate
	for _, p := range projects {
		for _, u := range people {
			cands = append(cands, reviewModel.Candidate{ImportID: job, Provider: importModels.ProviderTodoist, ProjectUUID: p, UserID: u})
		}
	}
	added, err := reviewModel.Add(ctx, cands)
	if err != nil || added != int64(len(cands)) {
		t.Fatalf("writing %d reviews added %d: %v", len(cands), added, err)
	}
	var n int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM import_member_reviews`).Scan(&n); err != nil || n != len(cands) {
		t.Fatalf("%d reviews stored (%v), want %d", n, err, len(cands))
	}
	if again, err := reviewModel.Add(ctx, cands); err != nil || again != 0 {
		t.Fatalf("writing them again added %d (%v), want none", again, err)
	}

	// A project's 150 people come a page at a time, each once.
	seen := map[uuid.UUID]bool{}
	var after *uuid.UUID
	for pages := 1; ; pages++ {
		page, more, err := reviewModel.People(ctx, projects[0], after)
		if err != nil || len(page) == 0 || len(page) > reviewModel.PeoplePage || pages > 3 {
			t.Fatalf("page %d: %d people, more %v (%v)", pages, len(page), more, err)
		}
		for _, r := range page {
			if seen[r.ID] {
				t.Fatalf("%s came on two pages", r.UserName)
			}
			seen[r.ID] = true
		}
		if !more {
			break
		}
		last := page[len(page)-1].ID
		after = &last
	}
	if len(seen) != len(people) {
		t.Fatalf("the pages held %d of the project's %d people", len(seen), len(people))
	}
}

// An import's people are candidates only if its first run started before the
// importers' fix; with no fix recorded, every import's are.
func TestOnlyImportsFirstRunBeforeTheFixAreCandidates(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	maker, ann, project := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{maker, ann} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, u, u.String()+"@example.com", u.String()[:8], "Someone"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, maker); err != nil {
		t.Fatal(err)
	}
	firstRun := time.Now().Add(-24 * time.Hour)
	var job uuid.UUID
	if err := env.PG.QueryRow(`INSERT INTO import_jobs (provider, source_workspace_name, source, started_at)
		VALUES ('todoist', 'Old workspace', 'api', $1) RETURNING id`, firstRun).Scan(&job); err != nil {
		t.Fatal(err)
	}
	for kind, id := range map[string]uuid.UUID{"project": project, "user": ann} {
		if _, err := env.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, $2, $3, $4)`, job, kind, kind+"-1", id); err != nil {
			t.Fatal(err)
		}
	}
	providers := []string{importModels.ProviderTodoist}
	for name, c := range map[string]struct {
		before *time.Time
		want   int
	}{
		"no fix recorded":            {nil, 1},
		"fixed after its first run":  {fixAt(firstRun.Add(time.Hour)), 1},
		"fixed before its first run": {fixAt(firstRun.Add(-time.Hour)), 0},
	} {
		got, err := reviewModel.Candidates(ctx, providers, c.before)
		if err != nil || len(got) != c.want {
			t.Errorf("%s: %d candidates (%v), want %d", name, len(got), err, c.want)
		}
	}
}

// fixAt is a moment the importers were fixed at, as Candidates takes it.
func fixAt(t time.Time) *time.Time { return &t }
