//go:build integration

package business

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The sync reads a pull request's reviews as GitHub decides them (V-986).
// GitHub lists every review the pull request had, oldest first, a reviewer's
// replies on threads among them (as COMMENTED reviews), a dismissed one as
// DISMISSED and the token holder's unsent one as PENDING. The sync wrote the
// whole list in one INSERT, which Postgres refuses when a reviewer has two
// reviews, and dropped the error: the task kept no review at all.
//
// Run: PKGS=./business/GitHub/ integ_rel.sh <tree> <log> '^TestTheSyncReadsAPullRequestsReviewsAsGitHubDecides$'
func TestTheSyncReadsAPullRequestsReviewsAsGitHubDecides(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user, project, task := uuid.New(), uuid.New(), uuid.New()
	const address = "https://github.com/acme/prsync/pull/5"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, []any{project, user}},
		{`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 5)`, []any{task, project, user, address}},
		// What the webhook stored before the sync: bo's request for changes
		// (its dismissal's delivery never came) and eve's approval.
		{`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state) VALUES ($1, $2, 'bo', 'CHANGES_REQUESTED'), ($1, $2, 'eve', 'APPROVED')`, []any{task, address}},
		// fay's reply is stored (its delivery came first); the list holds the
		// approval before it, which the reply leaves standing.
		{`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at) VALUES ($1, $2, 'fay', 'COMMENTED', '2026-11-13T09:05:00Z')`, []any{task, address}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	// ann approves, then replies; bo asked for changes, was dismissed, then
	// commented (the dismissal clears what was stored); dee asks for changes,
	// then comments; cy hasn't sent hers; the list holds only eve's replies,
	// which keep her stored approval.
	reviews := `[
		{"id": 101, "state": "APPROVED", "submitted_at": "2026-11-13T09:01:00Z", "user": {"login": "fay"}},
		{"id": 102, "state": "COMMENTED", "submitted_at": "2026-11-13T09:05:00Z", "user": {"login": "fay"}},
		{"id": 103, "state": "APPROVED", "user": {"login": "ann"}},
		{"id": 104, "state": "DISMISSED", "user": {"login": "bo"}},
		{"id": 105, "state": "CHANGES_REQUESTED", "user": {"login": "dee"}},
		{"id": 106, "state": "COMMENTED", "user": {"login": "ann"}},
		{"id": 107, "state": "COMMENTED", "user": {"login": "bo"}},
		{"id": 108, "state": "COMMENTED", "user": {"login": "eve"}},
		{"id": 109, "state": "COMMENTED", "user": {"login": "dee"}},
		{"id": 110, "state": "COMMENTED", "user": {"login": "eve"}},
		{"id": 111, "state": "PENDING", "user": {"login": "cy"}}]`
	answers := map[string]string{
		"/repos/acme/prsync/pulls/5":                 `{"state": "open", "html_url": "` + address + `", "head": {"sha": "abc5"}}`,
		"/repos/acme/prsync/commits/abc5/check-runs": `{"total_count": 0, "check_runs": []}`,
		"/repos/acme/prsync/pulls/5/reviews":         reviews,
	}
	was := githubHTTPClient
	githubHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, ok := answers[r.URL.Path]
		status := http.StatusOK
		if !ok {
			status, body = http.StatusNotFound, `{"message": "Not Found"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { githubHTTPClient = was })

	if err := syncPRDetailsFromGitHub(ctx, task, address, "acme", "prsync", 5, "test-token", nil, nil); err != nil {
		t.Fatal(err)
	}

	// What the task reads: each reviewer's reviews, folded (a review stored
	// before the sync among them).
	got := map[string]string{}
	stored, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range githubPRReviewDomain.Standing(stored) {
		got[r.GitHubLogin] = r.ReviewState
	}
	want := map[string]string{"ann": "APPROVED", "bo": "COMMENTED", "dee": "CHANGES_REQUESTED", "eve": "APPROVED", "fay": "APPROVED"}
	if len(got) != len(want) {
		t.Errorf("the task's reviews: %v, want %v", got, want)
	}
	for login, state := range want {
		if got[login] != state {
			t.Errorf("%s's review: %q, want %q", login, got[login], state)
		}
	}
	var decision string
	if err := env.PG.QueryRow(`SELECT COALESCE(github_pr_review_state, '') FROM tasks WHERE id = $1`, task).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "changes_requested" {
		t.Errorf("the task reads %q, want changes_requested (dee still asks for changes)", decision)
	}

	// A task holding the pull request's address as someone pasted it (a
	// trailing slash) reads the same decision after a sync: the sync writes
	// the reviews under the task's own address, which its read compares, not
	// under GitHub's spelling of it.
	pasted := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 5)`, pasted, project, user, address+"/"); err != nil {
		t.Fatal(err)
	}
	if err := syncPRDetailsFromGitHub(ctx, pasted, address+"/", "acme", "prsync", 5, "test-token", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.PG.QueryRow(`SELECT COALESCE(github_pr_review_state, '') FROM tasks WHERE id = $1`, pasted).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "changes_requested" {
		t.Errorf("the task holding %s/ reads %q after a sync, want changes_requested", address, decision)
	}
}
