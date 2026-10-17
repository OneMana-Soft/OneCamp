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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
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
		// Not synced since: the pull request attached a moment ago marked the
		// task synced, and an issue event no newer than that is skipped as its
		// echo, which would leave the task unmoved whatever the links say.
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/quiet/issues/9', github_issue_number = 9,
			github_last_synced_at = NULL WHERE id = $1`, w.task)
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

	// Every event stopped at whichever project's link to its repository a
	// lookup returned first: when that link didn't sync what the event is
	// about, the event was dropped, even for a task whose own project's link
	// syncs it, and so was what an event does without a task (a task made for
	// a new issue, a review or a comment told to the workspace).
	quiet := func(project, repo string, autoCreate bool) {
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, auto_create_tasks, automation_rules, created_by)
			VALUES ($1, 'acme', $2, false, false, $3, '{}'::jsonb, $4)`, project, repo, autoCreate, w.people["ana"])
	}

	t.Run("an issue event reaches its task past another project's link that doesn't sync issues", func(t *testing.T) {
		// Billing linked acme/quiet2 first and doesn't sync its issues; Website does.
		link(w.billing, "quiet2", false, "")
		link(w.project, "quiet2", true, "")
		// Not synced since: the pull request attached a moment ago marked it
		// synced, and an issue event no newer than that is taken for its echo.
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/quiet2/issues/11', github_issue_number = 11,
			github_last_synced_at = NULL WHERE id = $1`, w.task)
		event(t, "issues", "quiet2", closedIssue("quiet2", 11))
		eventually(t, "Website's own link moves the task to done", func() bool { return status(t, w.task) == "done" })
	})

	t.Run("a pull request reaches its task past another project's link that doesn't sync them", func(t *testing.T) {
		// Billing linked acme/quiet3 first, syncing nothing; Website syncs it.
		quiet(w.billing, "quiet3", false)
		link(w.project, "quiet3", true, "")
		event(t, "pull_request", "quiet3", pullRequest("quiet3", 21, "tidy-export", "Closes https://team.example.test/app/task/"+w.third))
		if _, pr := w.links(w.third); pr != "https://github.com/acme/quiet3/pull/21" {
			t.Errorf("the pull request wasn't attached: %q", pr)
		}
	})

	t.Run("a new issue's task is made by the first project's link that syncs issues and makes tasks", func(t *testing.T) {
		// A task is made in its project's team.
		var website struct {
			Q []struct {
				Uid string `json:"uid"`
			} `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(project_uuid, "`+w.project+`")) { uid } }`), &website); err != nil || len(website.Q) != 1 {
			t.Fatalf("Website isn't in the graph (%v)", err)
		}
		dg.Mutate(t, []map[string]any{{"uid": website.Q[0].Uid, "project_team": map[string]any{"uid": "_:web", "dgraph.type": "Team",
			"team_uuid": uuid.NewString(), "team_name": "Web", "team_deleted_at": "0001-01-01T00:00:00Z"}}})
		// Billing linked acme/quiet5 first and makes tasks, but syncs nothing.
		quiet(w.billing, "quiet5", true)
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, auto_create_tasks, automation_rules, created_by)
			VALUES ($1, 'acme', 'quiet5', true, true, true, '{}'::jsonb, $2)`, w.project, w.people["ana"])
		event(t, "issues", "quiet5", map[string]any{"action": "opened", "sender": map[string]any{"login": "stranger"}, "issue": map[string]any{
			"number": 1, "title": "The export times out", "html_url": "https://github.com/acme/quiet5/issues/1", "state": "open",
			"updated_at": time.Now().UTC().Format(time.RFC3339)}})
		var project string
		if err := env.PG.QueryRow(`SELECT project_id FROM tasks WHERE github_issue_url = 'https://github.com/acme/quiet5/issues/1'`).Scan(&project); err != nil || project != w.project {
			t.Errorf("the new issue's task is in project %q (%v), want Website's", project, err)
		}
	})

	t.Run("what an event does without a task needs some project's link to sync it", func(t *testing.T) {
		var mu sync.Mutex
		told := map[string]bool{}
		webhookBusiness.RegisterEventListener(func(_ context.Context, kind string, data map[string]interface{}) {
			repo, _ := data["repo"].(string)
			if address, _ := data["pr_url"].(string); repo == "" && strings.HasPrefix(address, "https://github.com/acme/") {
				repo = strings.Split(strings.TrimPrefix(address, "https://github.com/acme/"), "/")[0]
			}
			if strings.HasPrefix(repo, "quiet") {
				mu.Lock()
				told[kind+" "+repo] = true
				mu.Unlock()
			}
		})
		heard := func(what string) bool {
			mu.Lock()
			defer mu.Unlock()
			return told[what]
		}
		review := func(repo string, number int) map[string]any {
			return map[string]any{"action": "submitted", "review": map[string]any{"state": "commented", "body": "Looks fine",
				"user": map[string]any{"login": "stranger"}}, "pull_request": map[string]any{"number": number,
				"html_url": fmt.Sprintf("https://github.com/acme/%s/pull/%d", repo, number), "head": map[string]any{"ref": "review-me"}}}
		}
		comment := func(repo string, number int) map[string]any {
			address := fmt.Sprintf("https://github.com/acme/%s/pull/%d", repo, number)
			return map[string]any{"action": "created", "sender": map[string]any{"type": "User", "login": "stranger"},
				"comment": map[string]any{"id": 4000 + number, "body": "Could this skip empty rows?", "user": map[string]any{"login": "stranger"}},
				"issue":   map[string]any{"number": number, "html_url": address, "pull_request": map[string]any{"html_url": address}}}
		}
		// Billing alone links acme/quiet4, syncing nothing; acme/quiet3 is
		// linked by Billing first, syncing nothing, then by Website.
		quiet(w.billing, "quiet4", false)
		event(t, "pull_request_review", "quiet4", review("quiet4", 3))
		event(t, "issue_comment", "quiet4", comment("quiet4", 3))
		event(t, "pull_request_review", "quiet3", review("quiet3", 22))
		event(t, "issue_comment", "quiet3", comment("quiet3", 22))
		eventually(t, "the review on acme/quiet3 is told", func() bool { return heard("github.pr.review_submitted quiet3") })
		eventually(t, "the comment on acme/quiet3's pull request is told", func() bool { return heard("github.pr.comment quiet3") })
		for _, what := range []string{"github.pr.review_submitted quiet4", "github.pr.comment quiet4"} {
			if heard(what) {
				t.Errorf("%s was told, though no link to the repository syncs it", what)
			}
		}
	})

	t.Run("a delivery verifies with the secret of any project's link to its repository", func(t *testing.T) {
		// Each link has a secret of its own, and the repository's one webhook is
		// signed with the secret of whichever link registered it last.
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, webhook_secret, created_by) VALUES ($1, 'acme', 'signed', 'billing-secret', $2)`, w.billing, w.people["ana"])
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, webhook_secret, created_by) VALUES ($1, 'acme', 'signed', 'website-secret', $2)`, w.project, w.people["ana"])
		body := []byte(`{"repository":{"full_name":"acme/signed","name":"signed","owner":{"login":"acme"}}}`)
		signed := func(secret string) string {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			return "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		for _, secret := range []string{"website-secret", "billing-secret"} {
			if ok, _ := githubBusiness.VerifyGitHubWebhookSignature(ctx, body, signed(secret), "push"); !ok {
				t.Errorf("a delivery signed with %s was refused", secret)
			}
		}
		if ok, _ := githubBusiness.VerifyGitHubWebhookSignature(ctx, body, signed("guessed"), "push"); ok {
			t.Error("a delivery signed with a secret no link has was verified")
		}
	})

	// A branch's name is whoever pushes it's to choose: a fork's pull request
	// comes from "main" or "patch-1", which a task of another project may
	// carry too. Looked up across the workspace, that task was found first,
	// refused, and the event never tried its next lead (the pull request's
	// body, its address, a task named in the branch's name).
	billingTask := func(branch string) string {
		id := uuid.NewString()
		w.exec(`INSERT INTO tasks (id, project_id, created_by, github_branch) VALUES ($1, $2, $3, $4)`, id, w.billing, w.people["ana"], branch)
		return id
	}
	hotfix := billingTask("hotfix-x")
	billingTask("main")

	t.Run("a pull request from a branch another project's task carries attaches to the task its body names", func(t *testing.T) {
		event(t, "pull_request", "web", pullRequest("web", 15, "hotfix-x", "Closes https://team.example.test/app/task/"+w.task))
		if _, pr := w.links(w.task); pr != "https://github.com/acme/web/pull/15" {
			t.Errorf("the pull request wasn't attached: %q", pr)
		}
	})

	t.Run("a review from a branch another project's task carries reaches the task by its pull request", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/acme/web/pull/16', github_pr_number = 16, github_branch = NULL WHERE id = $1`, w.other)
		event(t, "pull_request_review", "web", map[string]any{"action": "submitted",
			"review":       map[string]any{"state": "commented", "body": "One question", "user": map[string]any{"login": "stranger"}},
			"pull_request": map[string]any{"number": 16, "html_url": "https://github.com/acme/web/pull/16", "head": map[string]any{"ref": "main"}}})
		var reviews int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM github_pr_reviews WHERE task_id = $1`, w.other).Scan(&reviews); err != nil || reviews != 1 {
			t.Errorf("the task has %d reviews (%v), want the one submitted", reviews, err)
		}
	})

	t.Run("a task the event may not change doesn't stop it trying the next lead", func(t *testing.T) {
		// Billing links acme/forks without syncing its pull requests, so its
		// task on main is the repository's project's, and refused.
		quiet(w.billing, "forks", false)
		link(w.project, "forks", true, "")
		event(t, "pull_request", "forks", pullRequest("forks", 2, "main", "Closes https://team.example.test/app/task/"+w.third))
		if _, pr := w.links(w.third); pr != "https://github.com/acme/forks/pull/2" {
			t.Errorf("the pull request wasn't attached: %q", pr)
		}
	})

	t.Run("a new branch named after a task attaches to it though another project's task carries it", func(t *testing.T) {
		named := "task/" + w.third + "-hotfix"
		w.exec(`UPDATE tasks SET github_branch = $2 WHERE id = $1`, hotfix, named)
		event(t, "create", "web", map[string]any{"ref": named, "ref_type": "branch"})
		if b := branch(t, w.third); b != named {
			t.Errorf("the new branch wasn't attached: %q", b)
		}
	})

	// A branch deleted on GitHub (once its pull request is merged, usually)
	// stayed on its tasks: it went on claiming any later branch of that name,
	// and a pull request opened from the task named a branch that was gone.
	t.Run("a branch deleted on GitHub is taken off the tasks that carried it", func(t *testing.T) {
		carrying := func(project, prURL string) string {
			id := uuid.NewString()
			w.exec(`INSERT INTO tasks (id, project_id, created_by, github_branch, github_pr_url) VALUES ($1, $2, $3, 'release-1', NULLIF($4, ''))`,
				id, project, w.people["ana"], prURL)
			return id
		}
		// On Website's acme/web: one task whose pull request is there, one with
		// none; one whose pull request, and so its branch, is in Website's
		// acme/quiet3; and a Billing task, whose project doesn't link acme/web.
		merged, bare := carrying(w.project, "https://github.com/acme/web/pull/40"), carrying(w.project, "")
		elsewhere, billingOne := carrying(w.project, "https://github.com/acme/quiet3/pull/41"), carrying(w.billing, "")
		event(t, "delete", "web", map[string]any{"ref": "release-1", "ref_type": "tag"})
		for _, task := range []string{merged, bare} {
			if b := branch(t, task); b != "release-1" {
				t.Errorf("deleting a tag of that name took the branch off task %s (now %q)", task, b)
			}
		}
		event(t, "delete", "web", map[string]any{"ref": "release-1", "ref_type": "branch"})
		for _, task := range []string{merged, bare} {
			if b := branch(t, task); b != "" {
				t.Errorf("task %s still carries the deleted branch %q", task, b)
			}
		}
		if _, pr := w.links(merged); pr != "https://github.com/acme/web/pull/40" {
			t.Errorf("the merged task's pull request went with its branch (now %q)", pr)
		}
		for _, task := range []string{elsewhere, billingOne} {
			if b := branch(t, task); b != "release-1" {
				t.Errorf("a branch of that name elsewhere was taken off task %s (now %q)", task, b)
			}
		}
	})
}
