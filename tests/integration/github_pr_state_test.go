//go:build integration
// +build integration

package integration_test

// A pull request's state, its checks and its review reach the task it names
// (V-705). Every one of those writes went through one UPDATE whose parameters
// Postgres couldn't type (42P08 "could not determine data type of parameter
// $4"), so each failed and was only logged, and a task never showed its pull
// request as draft, ready, merged, checked or reviewed. Every delivery comes
// through the signed webhook, for a repository of this test's own
// (acme/prstate): the links are cached for five minutes, and other tests
// deliver for acme/web.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestAPullRequestsStateChecksAndReviewReachItsTask$'

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAPullRequestsStateChecksAndReviewReachItsTask(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	recordGitHub(t)

	// Website (key PRS) links acme/prstate, which syncs pull requests; its
	// "Fix login" is PRS-1.
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'prstate', true, true, 'prstate-secret', $2)`, w.project, w.people["ana"])
	w.exec(`INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'PRS', 2)`, w.project)
	w.exec(`UPDATE tasks SET task_number = 1 WHERE id = $1`, w.task)

	const address = "https://github.com/acme/prstate/pull/7"
	at, sent := time.Now(), 0
	send := func(t *testing.T, event string, payload map[string]any) {
		t.Helper()
		sent++
		delivery := fmt.Sprintf("prstate-%d", sent)
		if code := w.deliver("acme/prstate", event, delivery, "prstate-secret", payload); code != http.StatusOK {
			t.Fatalf("the %s delivery: %d, want 200", event, code)
		}
		w.handled(t, delivery)
	}
	// Each pull request delivery is newer than the last, so none reads as an
	// echo of OneCamp's own sync.
	pr := func(action, state string, draft, merged bool) map[string]any {
		at = at.Add(time.Minute)
		return map[string]any{"action": action, "pull_request": map[string]any{
			"number": 7, "title": "PRS-1 Fix the login redirect", "html_url": address, "state": state, "draft": draft, "merged": merged,
			"user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": "prs-1-login"},
			"updated_at": at.UTC().Format(time.RFC3339)}}
	}
	type onTask struct {
		URL, State, Checks, Review string
		Draft                      bool
	}
	read := func(t *testing.T) onTask {
		t.Helper()
		var got onTask
		if err := env.PG.QueryRow(`SELECT COALESCE(github_pr_url, ''), COALESCE(github_pr_state, ''), COALESCE(github_pr_check_status, ''),
			COALESCE(github_pr_review_state, ''), COALESCE(github_pr_is_draft, false) FROM tasks WHERE id = $1`, w.task).
			Scan(&got.URL, &got.State, &got.Checks, &got.Review, &got.Draft); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Each step moves its own field and keeps every other one.
	steps := []struct {
		name, event string
		payload     map[string]any
		want        onTask
	}{
		{"a draft pull request opens", "pull_request", pr("opened", "open", true, false),
			onTask{URL: address, State: "open", Draft: true}},
		{"it's made ready for review", "pull_request", pr("ready_for_review", "open", false, false),
			onTask{URL: address, State: "open"}},
		{"its checks pass", "check_run", map[string]any{"action": "completed", "check_run": map[string]any{
			"name": "ci", "status": "completed", "conclusion": "success", "head_sha": "abc7",
			"html_url": "https://github.com/acme/prstate/runs/1", "pull_requests": []map[string]any{{"number": 7}}}},
			onTask{URL: address, State: "open", Checks: "success"}},
		{"a reviewer approves it", "pull_request_review", map[string]any{"action": "submitted", "review": map[string]any{
			"state": "approved", "body": "Looks right", "html_url": address + "#review-1", "submitted_at": time.Now().UTC().Format(time.RFC3339),
			"user": map[string]any{"login": "reviewer"}}, "pull_request": map[string]any{"number": 7, "html_url": address, "head": map[string]any{"ref": "prs-1-login"}}},
			onTask{URL: address, State: "open", Checks: "success", Review: "approved"}},
		{"it merges", "pull_request", pr("closed", "closed", false, true),
			onTask{URL: address, State: "merged", Checks: "success", Review: "approved"}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			send(t, step.event, step.payload)
			if got := read(t); got != step.want {
				t.Errorf("the task's pull request after %q: %+v, want %+v", step.name, got, step.want)
			}
		})
	}
}
