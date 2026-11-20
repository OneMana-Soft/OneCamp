//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
)

// The task's reviews endpoint (/github-pr-reviews/{task_uuid}) answers as it
// did with one row per reviewer (V-1060 keeps a row per review and folds on
// read): each reviewer's standing review, newest first, a dismissed one left
// out, in whichever order the deliveries arrive. ann's approval stands
// through her reply delivered first; bo's request for changes is dismissed;
// cy replies; dee's approval arrives after her later request for changes;
// eve's request for changes is dismissed and she approves.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestTheReviewsEndpointListsEachReviewersStandingReviewNewestFirst$'
func TestTheReviewsEndpointListsEachReviewersStandingReviewNewestFirst(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	recordGitHub(t)
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'prrev', true, true, 'prrev-secret', $2)`, w.project, w.people["ana"])
	address := func(number int) string { return fmt.Sprintf("https://github.com/acme/prrev/pull/%d", number) }
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)

	type delivery struct {
		action, state, login string
		id, minute           int
	}
	// In the order they arrive; a dismissal names its review's id and time.
	arrivals := []delivery{
		{"submitted", "commented", "ann", 4, 4}, {"submitted", "approved", "ann", 1, 1},
		{"submitted", "changes_requested", "bo", 2, 2}, {"dismissed", "dismissed", "bo", 2, 2},
		{"submitted", "commented", "cy", 3, 3},
		{"submitted", "changes_requested", "dee", 6, 6}, {"submitted", "approved", "dee", 5, 5},
		{"submitted", "changes_requested", "eve", 7, 7}, {"dismissed", "dismissed", "eve", 7, 7},
		{"submitted", "approved", "eve", 8, 8},
	}
	const want = "eve APPROVED, dee CHANGES_REQUESTED, cy COMMENTED, ann APPROVED"
	sent := 0
	for i, c := range []struct {
		name     string
		task     string
		reversed bool
	}{{"as they came", w.task, false}, {"in the reverse order", w.other, true}} {
		t.Run(c.name, func(t *testing.T) {
			number, branch := 30+i, fmt.Sprintf("rv-%d", i)
			w.exec(`UPDATE tasks SET github_branch = $2, github_pr_number = $3, github_pr_url = $4 WHERE id = $1`, c.task, branch, number, address(number))
			order := slices.Clone(arrivals)
			if c.reversed {
				slices.Reverse(order)
			}
			for _, d := range order {
				sent++
				name := fmt.Sprintf("prrev-%d", sent)
				payload := map[string]any{"action": d.action, "review": map[string]any{
					"id": d.id, "state": d.state, "body": "", "submitted_at": base.Add(time.Duration(d.minute) * time.Minute).Format(time.RFC3339),
					"user": map[string]any{"login": d.login}},
					"pull_request": map[string]any{"number": number, "html_url": address(number), "head": map[string]any{"ref": branch}}}
				if code := w.deliver("acme/prrev", "pull_request_review", name, "prrev-secret", payload); code != http.StatusOK {
					t.Fatalf("delivery %s: %d, want 200", name, code)
				}
				w.handled(t, name)
			}
			rc := chi.NewRouteContext()
			rc.URLParams.Add("task_uuid", c.task)
			rec := httptest.NewRecorder()
			taskController.GetGitHubPRReviews(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(w.as("ana"), chi.RouteCtxKey, rc)))
			if rec.Code != http.StatusOK {
				t.Fatalf("the reviews: %d %s, want 200", rec.Code, rec.Body.String())
			}
			var body struct {
				Reviews []struct {
					Login string `json:"github_login"`
					State string `json:"review_state"`
				} `json:"reviews"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range body.Reviews {
				got = append(got, r.Login+" "+r.State)
			}
			if strings.Join(got, ", ") != want {
				t.Errorf("the reviews endpoint answers %q, want %q", strings.Join(got, ", "), want)
			}
		})
	}
}
