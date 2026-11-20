//go:build integration

package business

// A review gives a task its pull request, with its state and draft, only
// while the task holds none (no address, a blank one or one of spaces), in
// one statement (CR-347's N3): an opened pull request's handler that set the
// task's pull request between the review's read and its write keeps it, and
// its state.
//
// Run: PKGS=./business/GitHub/ integ_rel.sh <tree> <log> '^TestATaskTakesAReviewedPullRequestOnlyWhileItHoldsNone$'

import (
	"context"
	"testing"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestATaskTakesAReviewedPullRequestOnlyWhileItHoldsNone(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	const opened, reviewed = "https://github.com/acme/web/pull/5", "https://github.com/acme/web/pull/7"
	user, project := uuid.New(), uuid.New()
	none, blank, spaces, holding := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, []any{project, user}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, []any{none, project, user}},
		{`INSERT INTO tasks (id, project_id, created_by, github_pr_url) VALUES ($1, $2, $3, '')`, []any{blank, project, user}},
		// reviewedPull reads an address of spaces as none (TrimSpace), so
		// the take does too.
		{`INSERT INTO tasks (id, project_id, created_by, github_pr_url) VALUES ($1, $2, $3, '  ')`, []any{spaces, project, user}},
		// The opened handler set this one's pull request first.
		{`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number, github_branch) VALUES ($1, $2, $3, $4, 5, 'b5')`, []any{holding, project, user, opened}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	for _, c := range []struct {
		name   string
		task   uuid.UUID
		took   bool
		holds  string
		number int
		state  string
		draft  bool
	}{
		{"a task holding none", none, true, reviewed, 7, "open", true},
		{"a task holding a blank address", blank, true, reviewed, 7, "open", true},
		{"a task holding an address of spaces", spaces, true, reviewed, 7, "open", true},
		{"a task holding another", holding, false, opened, 5, "", false},
	} {
		took, err := taskDomain.TakeGitHubPRIfNone(ctx, c.task, 7, reviewed, "b7", "open", true)
		if err != nil || took != c.took {
			t.Errorf("%s: took %v (%v), want %v", c.name, took, err, c.took)
		}
		var holds, state string
		var number int
		var draft bool
		if err := env.PG.QueryRow(`SELECT github_pr_url, COALESCE(github_pr_number, 0), COALESCE(github_pr_state, ''), COALESCE(github_pr_is_draft, false) FROM tasks WHERE id = $1`, c.task).Scan(&holds, &number, &state, &draft); err != nil {
			t.Fatal(err)
		}
		if holds != c.holds || number != c.number || state != c.state || draft != c.draft {
			t.Errorf("%s: holds %q #%d %q draft %v, want %q #%d %q draft %v", c.name, holds, number, state, draft, c.holds, c.number, c.state, c.draft)
		}
	}
}
