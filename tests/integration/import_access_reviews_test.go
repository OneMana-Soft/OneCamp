//go:build integration
// +build integration

package integration_test

// Migration 511 keeps the repair of what old Jira, Linear and Basecamp imports
// let every project member read: one review per import, project and gap, each
// change a re-check made with what was there before (so an admin can undo
// it), and why a person is on 510's list. It goes down and up again cleanly,
// and its rules hold: one review per gap, one change per item, only the words
// the repair uses, and a review's changes go with it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestImportAccessReviews' -v

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/tests/integration"
)

// migrationSQL is the one migration named name (its number aside), up or
// down.
func migrationSQL(t *testing.T, name, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_"+name+"."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one %s %s migration, found %v", name, dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestImportAccessReviewsMigrationUpDownUp(t *testing.T) {
	up, down := migrationSQL(t, "import_access_reviews", "up"), migrationSQL(t, "import_access_reviews", "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("Postgres 12 has no gen_random_uuid: ids come from uuid_generate_v4")
	}
	env := integration.SetupEnv(t)
	reasonColumn := func() bool {
		t.Helper()
		var ok bool
		if err := env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name = 'import_member_reviews' AND column_name = 'reason')`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, name := range []string{"import_access_reviews", "import_access_changes",
			"idx_fk_import_access_reviews_project_uuid", "idx_fk_import_access_reviews_decided_by",
			"idx_import_access_changes_entity_uuid"} {
			if got := tableExists(t, env.PG, name); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, name, got)
			}
		}
		if got := reasonColumn(); got != step.want {
			t.Fatalf("step %d: import_member_reviews.reason exists = %v", i, got)
		}
		if !tableExists(t, env.PG, "import_member_reviews") {
			t.Fatalf("step %d: 510's table went with it", i)
		}
	}
}

func TestImportAccessReviewsKeepTheirRules(t *testing.T) {
	env := integration.SetupEnv(t)
	one := func(q string, args ...any) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(q, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return id
	}
	refused := func(constraint, q string, args ...any) {
		t.Helper()
		_, err := env.PG.Exec(q, args...)
		if err == nil || !strings.Contains(err.Error(), constraint) {
			t.Errorf("%s was let in (%v), want it refused by %s", q, err, constraint)
		}
	}
	ana := one(`INSERT INTO users (id, email_id, username) VALUES (uuid_generate_v4(), 'ana@acme.test', 'ana') RETURNING id`)
	project := one(`INSERT INTO projects (id, project_name, created_by) VALUES (uuid_generate_v4(), 'Launch', $1) RETURNING id`, ana)
	job := one(`INSERT INTO import_jobs (provider, source_workspace_name, source) VALUES ('jira', 'acme.atlassian.net', 'api') RETURNING id`)

	const review = `INSERT INTO import_access_reviews (import_id, provider, project_uuid, gap, state) VALUES ($1, 'jira', $2, $3, $4) RETURNING id`
	security := one(review, job, project, "jira_security", "rechecking")
	one(review, job, project, "jira_comments", "needs_admin")
	refused("import_access_reviews_one_per_gap", review, job, project, "jira_security", "needs_admin")
	refused("import_access_reviews_gap_check", review, job, project, "everything", "rechecking")
	refused("import_access_reviews_state_check", review, job, project, "linear_teams", "opened")

	const change = `INSERT INTO import_access_changes (review_id, kind, entity_uuid, before, after) VALUES ($1, $2, $3, $4, $5) RETURNING id`
	task, comment := uuid.New(), uuid.New()
	one(change, security, "restricted", task, nil, `{"people":["`+ana.String()+`"]}`)
	one(change, security, "unassigned", task, `{"assignee":"`+ana.String()+`"}`, nil)
	one(change, security, "comment_hidden", comment, nil, nil)
	refused("import_access_changes_once", change, security, "restricted", task, nil, nil)
	refused("import_access_changes_kind_check", change, security, "opened", task, nil, nil)

	// A person on 510's list says why, in the one word the repair uses.
	member := one(`INSERT INTO import_member_reviews (import_id, provider, project_uuid, user_id) VALUES ($1, 'basecamp', $2, $3) RETURNING id`, job, project, ana)
	if _, err := env.PG.Exec(`UPDATE import_member_reviews SET reason = 'basecamp_client' WHERE id = $1`, member); err != nil {
		t.Errorf("a Basecamp client's reason: %v", err)
	}
	refused("import_member_reviews_reason_check", `UPDATE import_member_reviews SET reason = 'anyone' WHERE id = $1`, member)

	// A review's changes go with it, and a project's reviews with the project.
	if _, err := env.PG.Exec(`DELETE FROM import_access_reviews WHERE id = $1`, security); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM import_access_changes WHERE review_id = $1`, security); n != 0 {
		t.Errorf("%d changes outlived their review", n)
	}
	if _, err := env.PG.Exec(`DELETE FROM projects WHERE id = $1`, project); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM import_access_reviews WHERE project_uuid = $1`, project); n != 0 {
		t.Errorf("%d reviews outlived their project", n)
	}
}
