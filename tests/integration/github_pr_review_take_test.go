//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A task on a branch holding no pull request takes the reviewed one (V-1030)
// as an opened pull request's task does: with the pull request's state and
// draft, set by the same write the opened handler makes (CR-347's N1).
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestATaskTakingAReviewedPullRequestTakesItsStateAndDraft$'
func TestATaskTakingAReviewedPullRequestTakesItsStateAndDraft(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	recordGitHub(t)
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'prtake', true, true, 'prtake-secret', $2)`, w.project, w.people["ana"])
	address := func(number int) string { return fmt.Sprintf("https://github.com/acme/prtake/pull/%d", number) }

	for i, c := range []struct {
		name, task, branch, state string
		draft, merged             bool
		wantState                 string
	}{
		{"an open draft", w.other, "pt-5", "open", true, false, "open"},
		{"a merged pull request", w.third, "pt-6", "closed", false, true, "merged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			number := 5 + i
			w.exec(`UPDATE tasks SET github_branch = $2, github_pr_number = NULL, github_pr_url = NULL, github_pr_state = NULL, github_pr_is_draft = NULL WHERE id = $1`, c.task, c.branch)
			delivery := fmt.Sprintf("prtake-%d", number)
			payload := map[string]any{"action": "submitted",
				"review": map[string]any{"id": number, "state": "approved", "body": "", "user": map[string]any{"login": "ann"}},
				"pull_request": map[string]any{"number": number, "html_url": address(number), "head": map[string]any{"ref": c.branch},
					"state": c.state, "draft": c.draft, "merged": c.merged}}
			if code := w.deliver("acme/prtake", "pull_request_review", delivery, "prtake-secret", payload); code != http.StatusOK {
				t.Fatalf("the review delivery: %d, want 200", code)
			}
			w.handled(t, delivery)
			var prURL, state, decision sql.NullString
			var draft sql.NullBool
			if err := env.PG.QueryRow(`SELECT github_pr_url, github_pr_state, github_pr_is_draft, github_pr_review_state FROM tasks WHERE id = $1`, c.task).
				Scan(&prURL, &state, &draft, &decision); err != nil {
				t.Fatal(err)
			}
			if prURL.String != address(number) || decision.String != "approved" {
				t.Errorf("the task holds %q deciding %q, want %q deciding approved", prURL.String, decision.String, address(number))
			}
			if state.String != c.wantState || !draft.Valid || draft.Bool != c.draft {
				t.Errorf("the task's pull request reads %q, draft %v, want %q, draft %v", state.String, draft, c.wantState, c.draft)
			}
		})
	}
}
