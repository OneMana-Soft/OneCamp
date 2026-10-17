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
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
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
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
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
	http.DefaultTransport = issueStandIn{real: real}
	t.Cleanup(func() { http.DefaultTransport = real })

	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/web/issues/5', github_issue_number = 5,
		github_sync_status = NULL, github_sync_error = NULL WHERE id = $1`, w.task)

	// Ana, the project's admin, presses Refresh.
	rc := chi.NewRouteContext()
	rc.URLParams.Add("task_uuid", w.task)
	rec := httptest.NewRecorder()
	taskController.RefreshFromGitHub(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.WithValue(w.as("ana"), chi.RouteCtxKey, rc)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pressing Refresh: %d %s, want 202", rec.Code, rec.Body.String())
	}

	var status, reason string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_ = env.PG.QueryRow(`SELECT COALESCE(github_sync_status, ''), COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, w.task).Scan(&status, &reason)
		if status == "synced" || status == "failed" || time.Now().After(deadline) {
			break
		}
	}
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
