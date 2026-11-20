//go:build integration

// A review of a pull request reaches the task carrying its branch under the
// address the task holds (V-1030), moves the task by a rule only when it is
// a review of the task's own pull request (V-1028), and disconnecting GitHub
// leaves no task holding any of its pull request's state (V-1029).
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAReviewReachesItsTaskUnderTheAddressTheTaskHolds(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	recordGitHub(t)

	// Website links acme/prpull, which syncs pull requests.
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'prpull', true, true, 'prpull-secret', $2)`, w.project, w.people["ana"])

	at, sent := time.Now(), 0
	address := func(number int) string { return fmt.Sprintf("https://github.com/acme/prpull/pull/%d", number) }
	// review delivers login's review of state on pull request number, made
	// from branch, as a review of its own (GitHub sends each with its id).
	review := func(t *testing.T, state, login string, number int, branch string) {
		t.Helper()
		sent++
		at = at.Add(time.Minute)
		delivery := fmt.Sprintf("prpull-%d", sent)
		payload := map[string]any{"action": "submitted", "review": map[string]any{
			"id": sent, "state": state, "body": "", "submitted_at": at.UTC().Format(time.RFC3339), "user": map[string]any{"login": login}},
			"pull_request": map[string]any{"number": number, "html_url": address(number), "head": map[string]any{"ref": branch}}}
		if code := w.deliver("acme/prpull", "pull_request_review", delivery, "prpull-secret", payload); code != http.StatusOK {
			t.Fatalf("the review delivery: %d, want 200", code)
		}
		w.handled(t, delivery)
	}
	held := func(t *testing.T, task string) (decision, prURL string, number int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT COALESCE(github_pr_review_state, ''), COALESCE(github_pr_url, ''), COALESCE(github_pr_number, 0)
			FROM tasks WHERE id = $1`, task).Scan(&decision, &prURL, &number); err != nil {
			t.Fatal(err)
		}
		return decision, prURL, number
	}
	status := func(t *testing.T, task string) string {
		t.Helper()
		var out struct {
			Q []struct {
				Status string `json:"task_status"`
			} `json:"q"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(task_uuid, "`+task+`")) { task_status } }`), &out)
		if len(out.Q) != 1 {
			t.Fatalf("task %s isn't in the graph", task)
		}
		return out.Q[0].Status
	}

	t.Run("a task holding its pull request's address spelled otherwise keeps the decision", func(t *testing.T) {
		stored := "http://www.github.com/ACME/prpull/pull/21/files"
		w.exec(`UPDATE tasks SET github_branch = 'pp-21', github_pr_number = 21, github_pr_url = $2 WHERE id = $1`, w.task, stored)
		review(t, "approved", "ann", 21, "pp-21")
		decision, prURL, _ := held(t, w.task)
		if decision != "approved" {
			t.Errorf("the decision: %q, want approved (the review stood under GitHub's spelling, not the task's)", decision)
		}
		if prURL != stored {
			t.Errorf("the task's address: %q, want it as it was, %q", prURL, stored)
		}
	})

	t.Run("a task on the branch holding no pull request takes the reviewed one and its decision", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_branch = 'pp-22', github_pr_number = NULL, github_pr_url = NULL WHERE id = $1`, w.other)
		review(t, "approved", "ann", 22, "pp-22")
		decision, prURL, number := held(t, w.other)
		if prURL != address(22) || number != 22 {
			t.Errorf("the task's pull request: %q #%d, want %q #22", prURL, number, address(22))
		}
		if decision != "approved" {
			t.Errorf("the decision: %q, want approved", decision)
		}
	})

	t.Run("a review of an older pull request from the task's branch runs no rule", func(t *testing.T) {
		w.exec(`UPDATE github_links SET automation_rules = '{"approved":"done"}' WHERE repo_name = 'prpull'`)
		w.exec(`UPDATE tasks SET github_branch = 'pp-23', github_pr_number = 23, github_pr_url = $2 WHERE id = $1`, w.third, address(23))
		review(t, "changes_requested", "bo", 23, "pp-23")
		if decision, _, _ := held(t, w.third); decision != "changes_requested" {
			t.Fatalf("the decision before: %q, want changes_requested", decision)
		}

		review(t, "approved", "bo", 20, "pp-23")
		if got := status(t, w.third); got != "todo" {
			t.Errorf("the task's status after a review of the old pull request: %q, want todo (no rule runs)", got)
		}
		if decision, prURL, _ := held(t, w.third); decision != "changes_requested" || prURL != address(23) {
			t.Errorf("the task after a review of the old pull request: %q on %q, want changes_requested on %q", decision, prURL, address(23))
		}

		// The same review of the task's own pull request runs the rule.
		review(t, "approved", "bo", 23, "pp-23")
		if got := status(t, w.third); got != "done" {
			t.Errorf("the task's status after a review of its own pull request: %q, want done (the rule runs)", got)
		}
		if decision, _, _ := held(t, w.third); decision != "approved" {
			t.Errorf("the decision after a review of its own pull request: %q, want approved", decision)
		}
	})

	t.Run("disconnecting GitHub leaves no task holding any of its state", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_issue_number = 4, github_issue_url = 'https://github.com/acme/prpull/issues/4',
			github_pr_state = 'open', github_pr_check_status = 'success', github_pr_is_draft = true, github_last_synced_at = NOW() WHERE id = $1`, w.task)
		// A task holding only a pull request's state, its address gone.
		w.exec(`UPDATE tasks SET github_pr_number = NULL, github_pr_url = NULL, github_branch = NULL,
			github_pr_review_state = 'approved', github_pr_is_draft = true WHERE id = $1`, w.other)
		w.exec(`UPDATE integrations SET access_token = NULL WHERE provider = 'github'`) // nothing to revoke over the network
		if err := githubBusiness.DisconnectGitHub(ctx, w.people["ana"]); err != nil {
			t.Fatal(err)
		}
		for _, task := range []string{w.task, w.other, w.third} {
			var issueNumber, prNumber int
			var issueURL, prURL, branch, state, checks, decision string
			var draft, synced bool
			if err := env.PG.QueryRow(`SELECT COALESCE(github_issue_number, 0), COALESCE(github_issue_url, ''), COALESCE(github_pr_number, 0),
				COALESCE(github_pr_url, ''), COALESCE(github_branch, ''), COALESCE(github_pr_state, ''), COALESCE(github_pr_check_status, ''),
				COALESCE(github_pr_review_state, ''), COALESCE(github_pr_is_draft, false), github_last_synced_at IS NOT NULL
				FROM tasks WHERE id = $1`, task).Scan(&issueNumber, &issueURL, &prNumber, &prURL, &branch, &state, &checks, &decision, &draft, &synced); err != nil {
				t.Fatal(err)
			}
			if issueNumber != 0 || issueURL != "" || prNumber != 0 || prURL != "" || branch != "" || state != "" || checks != "" || decision != "" || draft || synced {
				t.Errorf("task %s after disconnecting: issue %d %q, pull request %d %q, branch %q, state %q, checks %q, decision %q, draft %v, synced %v; want none",
					task, issueNumber, issueURL, prNumber, prURL, branch, state, checks, decision, draft, synced)
			}
		}
	})
}
