//go:build integration
// +build integration

package integration_test

// Refreshing a task from GitHub, against real Postgres, Redis and Dgraph,
// through the real route, with GitHub's API stood in.
//
// "Refresh from GitHub" applies the issue's title, body, state, assignee and
// labels to its task. It acted as the task's creator, read from the full task
// read, which reads no creator, so every refresh stopped at "actor user not
// found": the task's sync read failed and nothing of GitHub's reached it, on
// every line. It now acts as whoever pressed Refresh, one of the project's
// admins.
//
// Run: go test -tags=integration ./tests/integration/ -run TestRefreshingATaskFromGitHubAppliesTheIssue -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// issueStandIn is GitHub's API for one issue, acme/web#5, and nothing else.
type issueStandIn struct{ real http.RoundTripper }

func (s issueStandIn) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return s.real.RoundTrip(r)
	}
	body := `{}`
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/issues/5":
		body = `{"number": 5, "title": "Fix login on Safari", "body": "", "state": "open", "labels": [], "assignee": null,
			"html_url": "https://github.com/acme/web/issues/5", "user": {"login": "octo"}}`
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		body = `[]`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestRefreshingATaskFromGitHubAppliesTheIssue(t *testing.T) {
	w, dg := refreshWorld(t, func(real http.RoundTripper) http.RoundTripper { return issueStandIn{real: real} })
	w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/web/issues/5', github_issue_number = 5,
		github_sync_status = NULL, github_sync_error = NULL WHERE id = $1`, w.task)

	// Ana, the project's admin, presses Refresh.
	status, reason := pressRefresh(t, w, w.task)
	if status != "synced" {
		t.Fatalf("the refresh ended %q (%q), want synced", status, reason)
	}

	var got struct {
		T []struct {
			Name       string `json:"task_name"`
			Activities []struct {
				By struct {
					Uuid string `json:"user_uuid"`
				} `json:"activity_by"`
				Next string `json:"activity_next_state"`
			} `json:"task_activities"`
		} `json:"t"`
	}
	raw := dg.Query(t, `{ t(func: eq(task_uuid, "`+w.task+`")) { task_name
		task_activities @filter(eq(activity_type, "nameUpdate")) { activity_by { user_uuid } activity_next_state } } }`)
	if err := json.Unmarshal(raw, &got); err != nil || len(got.T) != 1 {
		t.Fatalf("read the task back: %v %s", err, raw)
	}
	if got.T[0].Name != "Fix login on Safari" {
		t.Errorf("the task is called %q, want the issue's title", got.T[0].Name)
	}
	if a := got.T[0].Activities; len(a) != 1 || a[0].By.Uuid != w.people["ana"].String() || a[0].Next != "Fix login on Safari" {
		t.Errorf("the title's change was recorded as %+v, want one, by Ana, who pressed Refresh", a)
	}
}

// refreshWorld is a GitHub workspace (seedGitHubWorkspace) on real Postgres,
// Redis and Dgraph, with search and MQTT stood in and GitHub's API answered
// by api, which hands every other host to the real transport.
func refreshWorld(t *testing.T, api func(real http.RoundTripper) http.RoundTripper) (*githubWorkspace, *integration.DgraphEnv) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	real := http.DefaultTransport
	http.DefaultTransport = api(real)
	t.Cleanup(func() { http.DefaultTransport = real })
	return seedGitHubWorkspace(t, env, dg), dg
}

// pressRefresh is Ana, the project's admin, pressing Refresh on task through
// the route; it answers how the refresh ended, its sync status and reason.
func pressRefresh(t *testing.T, w *githubWorkspace, task string) (status, reason string) {
	t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("task_uuid", task)
	rec := httptest.NewRecorder()
	taskController.RefreshFromGitHub(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.WithValue(w.as("ana"), chi.RouteCtxKey, rc)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pressing Refresh: %d %s, want 202", rec.Code, rec.Body.String())
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_ = w.db.PG.QueryRow(`SELECT COALESCE(github_sync_status, ''), COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, task).Scan(&status, &reason)
		if status == "synced" || status == "failed" || time.Now().After(deadline) {
			return status, reason
		}
	}
}

// pullStandIn is GitHub's API for one pull request, acme/web#5, its owner and
// name in any case, as GitHub's API takes them: ann approves, then replies,
// and dee asks for changes.
type pullStandIn struct{ real http.RoundTripper }

func (s pullStandIn) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return s.real.RoundTrip(r)
	}
	body := `{}`
	switch path := strings.ToLower(r.URL.Path); {
	case r.Method != http.MethodGet:
	case path == "/repos/acme/web/issues/5":
		body = `{"number": 5, "title": "Fix login on Safari", "body": "", "state": "open", "labels": [], "assignee": null,
			"html_url": "https://github.com/acme/web/pull/5", "pull_request": {}, "user": {"login": "octo"}}`
	case path == "/repos/acme/web/pulls/5":
		body = `{"number": 5, "state": "open", "html_url": "https://github.com/acme/web/pull/5", "head": {"sha": "abc5"}}`
	case path == "/repos/acme/web/commits/abc5/check-runs":
		body = `{"total_count": 0, "check_runs": []}`
	case path == "/repos/acme/web/pulls/5/reviews":
		body = `[{"id": 1, "state": "APPROVED", "user": {"login": "ann"}}, {"id": 2, "state": "CHANGES_REQUESTED", "user": {"login": "dee"}},
			{"id": 3, "state": "COMMENTED", "user": {"login": "ann"}}]`
	case strings.HasSuffix(path, "/comments"):
		body = `[]`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// holdPullRequest has w's task hold acme/web#5 at address, with no decision
// and no refresh yet. Reviews kept under another address aren't its: its
// read compares the address it holds.
func holdPullRequest(t *testing.T, w *githubWorkspace, address string) {
	t.Helper()
	if _, err := w.db.PG.Exec(`UPDATE tasks SET github_issue_url = NULL, github_pr_url = $2, github_pr_number = 5,
		github_pr_review_state = NULL, github_sync_status = NULL, github_sync_error = NULL WHERE id = $1`, w.task, address); err != nil {
		t.Fatalf("holding %s: %v", address, err)
	}
}

// taskReads is what task shows of its pull request's reviews: its reviewers'
// standing reviews, as the task's read folds them, and the decision it keeps.
func taskReads(t *testing.T, w *githubWorkspace, task string) string {
	t.Helper()
	rs, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(context.Background(), uuid.MustParse(task))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range githubPRReviewDomain.Standing(rs) {
		got[r.GitHubLogin] = r.ReviewState
	}
	var decision string
	if err := w.db.PG.QueryRow(`SELECT COALESCE(github_pr_review_state, '') FROM tasks WHERE id = $1`, task).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%v %q", got, decision)
}

// Refreshing a pull request's task reads its reviewers' reviews and their
// decision however the task holds the pull request's address (V-986): the
// refresh hands the sync the task's own address, which the task's read
// compares, and the sync keeps the reviews under it. Under GitHub's spelling
// of it, a task holding the address as someone pasted it read no review.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestRefreshingAPullRequestsTaskReadsItsReviewsHoweverItHoldsTheAddress$'
func TestRefreshingAPullRequestsTaskReadsItsReviewsHoweverItHoldsTheAddress(t *testing.T) {
	w, _ := refreshWorld(t, func(real http.RoundTripper) http.RoundTripper { return pullStandIn{real: real} })
	for _, held := range []struct{ name, address string }{
		{"http", "http://github.com/acme/web/pull/5"},
		{"www", "https://www.github.com/acme/web/pull/5"},
		{"a trailing slash", "https://github.com/acme/web/pull/5/"},
		{"its files", "https://github.com/acme/web/pull/5/files"},
		{"another case", "HTTPS://GitHub.com/Acme/Web/pull/5"},
	} {
		t.Run(held.name, func(t *testing.T) {
			holdPullRequest(t, w, held.address)
			if status, reason := pressRefresh(t, w, w.task); status != "synced" {
				t.Fatalf("refreshing the task holding %s ended %q (%q), want synced", held.address, status, reason)
			}
			if got, want := taskReads(t, w, w.task), `map[ann:APPROVED dee:CHANGES_REQUESTED] "changes_requested"`; got != want {
				t.Errorf("the task holding %s reads %s after a refresh, want %s", held.address, got, want)
			}
		})
	}
}

// A pull request's address over 512 characters keeps none of its reviews
// (V-1041). A review is kept per task and address, and 597 gives a review its
// task's address only up to 512 characters, so a longer one is past what the
// key is sure to hold. The refresh reads the pull request, writes no review,
// leaves the task with no decision and still ends synced.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestAPullRequestAddressOver512CharactersKeepsNoReview$'
func TestAPullRequestAddressOver512CharactersKeepsNoReview(t *testing.T) {
	w, _ := refreshWorld(t, func(real http.RoundTripper) http.RoundTripper { return pullStandIn{real: real} })
	address := "https://github.com/acme/web/pull/5?from=" + strings.Repeat("a", 560)
	holdPullRequest(t, w, address)
	if status, reason := pressRefresh(t, w, w.task); status != "synced" {
		t.Fatalf("refreshing the task holding a %d-character address ended %q (%q), want synced", len(address), status, reason)
	}
	var kept int
	if err := w.db.PG.QueryRow(`SELECT COUNT(*) FROM github_pr_reviews WHERE task_id = $1`, w.task).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("a refresh kept %d reviews under a %d-character address, want none", kept, len(address))
	}
	if got := taskReads(t, w, w.task); got != `map[] ""` {
		t.Errorf("the task holding a %d-character address reads %s after a refresh, want no review", len(address), got)
	}
}

// pagedReviews is pullStandIn with pull request #5's reviews served the way
// GitHub serves them: per_page at a time (30 unless asked, 100 at most),
// oldest first, a Link header naming the next page; page failing (if any)
// answers 502.
type pagedReviews struct {
	pullStandIn
	reviews []string
	failing int
}

func (s *pagedReviews) RoundTrip(r *http.Request) (*http.Response, error) {
	const list = "https://api.github.com/repos/acme/web/pulls/5/reviews"
	if r.URL.Host != "api.github.com" || strings.ToLower(r.URL.Path) != "/repos/acme/web/pulls/5/reviews" {
		return s.pullStandIn.RoundTrip(r)
	}
	per, page := 30, 1
	if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil {
		per = min(n, 100)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		page = n
	}
	header := http.Header{"Content-Type": {"application/json"}}
	if page == s.failing {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: header,
			Body: io.NopCloser(strings.NewReader(`{"message": "Server Error"}`)), Request: r}, nil
	}
	from, to := min((page-1)*per, len(s.reviews)), min(page*per, len(s.reviews))
	if to < len(s.reviews) {
		header.Set("Link", fmt.Sprintf(`<%s?per_page=%d&page=%d>; rel="next", <%s?per_page=%d&page=%d>; rel="last"`,
			list, per, page+1, list, per, (len(s.reviews)+per-1)/per))
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header,
		Body: io.NopCloser(strings.NewReader("[" + strings.Join(s.reviews[from:to], ",") + "]")), Request: r}, nil
}

// aThread is n reviews a minute apart: ann's request for changes, a thread of
// replies on it (GitHub files each as a COMMENTED review, bo's and ann's
// in turn) and, last, ann's approval.
func aThread(n int) []string {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	review := func(i int, login, state string) string {
		return fmt.Sprintf(`{"id": %d, "state": %q, "submitted_at": %q, "user": {"login": %q}}`,
			i+1, state, at.Add(time.Duration(i)*time.Minute).Format(time.RFC3339), login)
	}
	reviews := []string{review(0, "ann", "CHANGES_REQUESTED")}
	for i := 1; i < n-1; i++ {
		reviews = append(reviews, review(i, []string{"bo", "ann"}[i%2], "COMMENTED"))
	}
	return append(reviews, review(n-1, "ann", "APPROVED"))
}

// A refresh reads every page of a pull request's reviews (V-986, V-1027).
// GitHub serves 30 a page unless asked, and every reply on a review thread is
// a review, so after a long thread a reviewer's approval sat past the first
// page and the task read the request for changes before it. 1,000 reviews are
// ten pages of 100, the most a refresh reads (asked without per_page=100 they
// are 34 pages of 30, and the refresh writes none); ann's approval is the last.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestARefreshReadsEveryPageOfAPullRequestsReviews$'
func TestARefreshReadsEveryPageOfAPullRequestsReviews(t *testing.T) {
	w, _ := refreshWorld(t, func(real http.RoundTripper) http.RoundTripper {
		return &pagedReviews{pullStandIn: pullStandIn{real: real}, reviews: aThread(1000)}
	})
	holdPullRequest(t, w, "https://github.com/acme/web/pull/5")
	if status, reason := pressRefresh(t, w, w.task); status != "synced" {
		t.Fatalf("the refresh ended %q (%q), want synced", status, reason)
	}
	if got, want := taskReads(t, w, w.task), `map[ann:APPROVED bo:COMMENTED] "approved"`; got != want {
		t.Errorf("ann approved last, on the tenth page: the task reads %s, want %s", got, want)
	}
}

// A refresh that can't read a pull request's reviews whole writes none of
// them: part of the list must not replace a decision a later review made. A
// page GitHub fails, or more pages than the sync reads (ten of 100).
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestARefreshThatReadsPartOfAPullRequestsReviewsWritesNone$'
func TestARefreshThatReadsPartOfAPullRequestsReviewsWritesNone(t *testing.T) {
	github := &pagedReviews{}
	w, _ := refreshWorld(t, func(real http.RoundTripper) http.RoundTripper {
		github.pullStandIn = pullStandIn{real: real}
		return github
	})
	for _, c := range []struct {
		name             string
		reviews, failing int
	}{
		{"the second of two pages fails", 130, 2},
		{"eleven pages", 1001, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			github.reviews, github.failing = aThread(c.reviews), c.failing
			holdPullRequest(t, w, "https://github.com/acme/web/pull/5")
			if status, reason := pressRefresh(t, w, w.task); status != "synced" {
				t.Fatalf("the refresh ended %q (%q), want synced", status, reason)
			}
			if got := taskReads(t, w, w.task); got != `map[] ""` {
				t.Errorf("%d reviews, page %d failing: the task reads %s, want no review", c.reviews, c.failing, got)
			}
		})
	}
}
