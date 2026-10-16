//go:build integration
// +build integration

package integration_test

// A pull request, a branch or a commit attaches only to a task in a project
// that linked its repository (PROJ-BUG-6).
//
// A pull request found its task by the branch name or by a task's address in
// its body, both written by whoever opens it. Opened on any linked repository
// (anyone may open one on a public repository) and naming a task in another
// project, it wrote itself onto that task, ran the repository's status rule
// on it as the person who linked the repository, and from then on the sync
// posted the task's comments, description and title to it. Commits naming a
// task added their message to its history, and a new branch named after one
// claimed it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPullRequestsClaimOnlyTheirProjectsTasks -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestPullRequestsClaimOnlyTheirProjectsTasks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	// A status change also writes the task's search entry.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	gh := recordGitHub(t)

	// Website (acme/web) holds the tasks; Billing (acme/billing) is another
	// project's repository. Both move a task to review when a pull request opens.
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`UPDATE github_links SET automation_rules = '{"pr_opened":"inReview"}'`)

	event := func(t *testing.T, kind, repo string, payload map[string]any) {
		t.Helper()
		payload["repository"] = map[string]any{"full_name": "acme/" + repo, "name": repo, "owner": map[string]any{"login": "acme"}}
		raw, _ := json.Marshal(payload)
		if err := githubBusiness.HandleGitHubWebhookEvent(ctx, kind, raw); err != nil {
			t.Fatalf("%s on acme/%s: %v", kind, repo, err)
		}
	}
	pullRequest := func(repo string, number int, branch, body string) map[string]any {
		return map[string]any{"action": "opened", "pull_request": map[string]any{
			"number": number, "title": "Tidy the export", "html_url": fmt.Sprintf("https://github.com/acme/%s/pull/%d", repo, number),
			"state": "open", "body": body, "user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": branch},
			"updated_at": time.Now().UTC().Format(time.RFC3339)}}
	}
	push := func(message string) map[string]any {
		return map[string]any{"ref": "refs/heads/main", "sender": map[string]any{"login": "stranger"},
			"commits": []map[string]any{{"message": message, "url": "https://github.com/acme/x/commit/abc", "id": "abc"}}}
	}
	closedIssue := func(repo string, number int) map[string]any {
		return map[string]any{"action": "closed", "sender": map[string]any{"login": "stranger"}, "issue": map[string]any{
			"number": number, "title": "Shared export", "html_url": fmt.Sprintf("https://github.com/acme/%s/issues/%d", repo, number),
			"state": "closed", "updated_at": time.Now().UTC().Format(time.RFC3339)}}
	}
	// The helpers below fail the test that calls them, so one subtest's
	// failure never stops the others.
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
	activity := func(t *testing.T, task string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT activity_type FROM github_task_activities WHERE task_id = $1 ORDER BY created_at`, task)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var kinds []string
		for rows.Next() {
			var k string
			_ = rows.Scan(&k)
			kinds = append(kinds, k)
		}
		return kinds
	}
	branch := func(t *testing.T, task string) string {
		t.Helper()
		var b string
		if err := env.PG.QueryRow(`SELECT COALESCE(github_branch, '') FROM tasks WHERE id = $1`, task).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	eventually := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	// link links a project to acme/repo with these status rules ("" for none).
	link := func(project, repo string, syncIssues bool, rules string) {
		if rules == "" {
			rules = "{}"
		}
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, automation_rules, created_by)
			VALUES ($1, 'acme', $2, $3, true, $4::jsonb, $5)`, project, repo, syncIssues, rules, w.people["ana"])
	}

	// Another project's repository naming the task, by its body, by the
	// branch the task already has, by a commit and by a new branch; then the
	// task's own repository, the same ways.
	w.exec(`UPDATE tasks SET github_branch = 'fix-billing' WHERE id = $1`, w.third)
	event(t, "pull_request", "billing", pullRequest("billing", 12, "stranger-patch-1", "Closes https://team.example.test/app/task/"+w.other))
	event(t, "pull_request", "billing", pullRequest("billing", 14, "fix-billing", "No address here"))
	event(t, "push", "billing", push("Fixes task/"+w.other))
	event(t, "create", "billing", map[string]any{"ref": "task/" + w.other + "-hotfix", "ref_type": "branch"})

	event(t, "pull_request", "web", pullRequest("web", 13, "fix-login", "Closes https://team.example.test/app/task/"+w.task))
	event(t, "push", "web", push("Touches task/"+w.task))
	event(t, "create", "web", map[string]any{"ref": "task/" + w.task + "-styles", "ref_type": "branch"})

	t.Run("a pull request on the task's own repository attaches and moves it", func(t *testing.T) {
		if _, pr := w.links(w.task); pr != "https://github.com/acme/web/pull/13" {
			t.Errorf("the pull request wasn't attached: %q", pr)
		}
		eventually(t, "the task's history shows the pull request and the commit", func() bool {
			got := strings.Join(activity(t, w.task), ",")
			return strings.Contains(got, "pr_opened") && strings.Contains(got, "commit_linked")
		})
		eventually(t, "the rule moves the task to review", func() bool { return status(t, w.task) == "inReview" })
		if b := branch(t, w.task); b != "task/"+w.task+"-styles" {
			t.Errorf("the new branch wasn't attached: %q", b)
		}
	})

	t.Run("another project's repository changes nothing on the task", func(t *testing.T) {
		for _, task := range []string{w.other, w.third} {
			if issue, pr := w.links(task); issue != "" || pr != "" {
				t.Errorf("task %s was linked to %q / %q", task, issue, pr)
			}
			if got := activity(t, task); len(got) != 0 {
				t.Errorf("task %s's history took %v", task, got)
			}
			if s := status(t, task); s != "todo" {
				t.Errorf("task %s was moved to %q", task, s)
			}
		}
		if b := branch(t, w.other); b != "" {
			t.Errorf("another project's branch was attached: %q", b)
		}
		if n := gh.count("/repos/acme/billing/"); n != 0 {
			t.Errorf("GitHub was called for acme/billing: %v", gh.calls())
		}
	})

	// An issue event finds its task by the issue's address, and acted through
	// the repository's link an event looks up: whichever project's link to it
	// came first. Each case has a repository of its own, so no event reads a
	// link cached by another case's.
	t.Run("an issue event acts on a task only through its own project's link", func(t *testing.T) {
		// Only Billing links acme/ops; a Website task is linked to an issue
		// there (as before the start-up repair unlinks it).
		link(w.billing, "ops", true, "")
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/ops/issues/7', github_issue_number = 7 WHERE id = $1`, w.third)
		event(t, "issues", "ops", closedIssue("ops", 7))
		if s := status(t, w.third); s != "todo" {
			t.Errorf("another project's link to the repository moved the task to %q", s)
		}
		if got := activity(t, w.third); len(got) != 0 {
			t.Errorf("another project's link to the repository wrote the task's history: %v", got)
		}
	})

	t.Run("the task's own project's link decides whether an issue event applies", func(t *testing.T) {
		// Both link acme/quiet; Billing syncs its issues, Website doesn't.
		link(w.billing, "quiet", true, "")
		link(w.project, "quiet", false, "")
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/quiet/issues/9', github_issue_number = 9 WHERE id = $1`, w.task)
		before := status(t, w.task)
		event(t, "issues", "quiet", closedIssue("quiet", 9))
		if s := status(t, w.task); s != before {
			t.Errorf("an issue Website doesn't sync moved its task from %q to %q", before, s)
		}
	})

	t.Run("the task's own project's link decides what an issue event does", func(t *testing.T) {
		// Both link acme/shared and sync its issues; Billing's rule moves a
		// closed issue's task to review, Website's has none, so to done.
		link(w.billing, "shared", true, `{"issue_closed":"inReview"}`)
		link(w.project, "shared", true, "")
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/shared/issues/8', github_issue_number = 8 WHERE id = $1`, w.other)
		event(t, "issues", "shared", closedIssue("shared", 8))
		eventually(t, "Website's own link moves the task to done", func() bool { return status(t, w.other) == "done" })
	})
}
