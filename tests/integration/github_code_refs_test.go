//go:build integration
// +build integration

package integration_test

// Code names a task by its key (P10-1a): "Fixes WEB-1" in a commit that lands
// on the repository's default branch, or in a pull request that merges, moves
// the task, as the integration's bot, and only a task of a project linked to
// that repository that the bot may read. Every delivery comes through the
// signed webhook, once.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestCodeLinksTasksByKeyThroughTheSignedWebhook$|^TestAGitHubDeliveryIsClaimedAgainOnlyAfterItFailedOrStalled$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	githubWebhookDeliveryDomain "github.com/akashc777/OneCamp/domain/GitHubWebhookDelivery"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCodeLinksTasksByKeyThroughTheSignedWebhook(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	mq := integration.RecordMqtt(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	recordGitHub(t)

	// Website (WEB, acme/web) holds WEB-1..3; WEB-3 is restricted to people
	// the bot isn't. Billing (BIL, acme/billing) holds BIL-1.
	w := seedGitHubWorkspace(t, env, dg)
	// Website's link makes a task of a pull request that opens naming none,
	// and its rules move a task when a pull request opens or merges (set
	// before any delivery: the links are cached for five minutes).
	w.exec(`UPDATE github_links SET webhook_secret = 'web-secret', auto_create_tasks = true,
		automation_rules = '{"pr_opened":"inProgress","pr_merged":"done"}' WHERE repo_name = 'web'`)
	w.exec(`INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'WEB', 4), ($2, 'BIL', 2)`, w.project, w.billing)
	for i, task := range []string{w.task, w.other, w.third} {
		w.exec(`UPDATE tasks SET task_number = $1 WHERE id = $2`, i+1, task)
	}
	theirs := uuid.NewString()
	w.exec(`INSERT INTO tasks (id, project_id, created_by, task_number) VALUES ($1, $2, $3, 1)`, theirs, w.billing, w.people["ana"])
	w.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, w.third, w.project, w.people["ana"])

	deliver := func(event, delivery, secret string, payload map[string]any) int {
		return w.deliver("acme/web", event, delivery, secret, payload)
	}
	push := func(branch, message string) map[string]any {
		return map[string]any{"ref": "refs/heads/" + branch, "sender": map[string]any{"login": "stranger"},
			"commits": []map[string]any{{"message": message, "url": "https://github.com/acme/web/commit/abc", "id": "abc"}}}
	}
	pr := func(action string, number int, title, body, branch string, at time.Time) map[string]any {
		return map[string]any{"action": action, "pull_request": map[string]any{
			"number": number, "title": title, "html_url": fmt.Sprintf("https://github.com/acme/web/pull/%d", number), "state": "open",
			"body": body, "user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": branch},
			"updated_at": at.UTC().Format(time.RFC3339)}}
	}
	handled := w.handled
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
	lines := func(t *testing.T, task, kind string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM github_task_activities WHERE task_id = $1 AND ($2 = '' OR activity_type = $2)`, task, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// changedBy is who made each status change in the task's history.
	changedBy := func(t *testing.T, task string) []string {
		t.Helper()
		var out struct {
			Q []struct {
				Activities []struct {
					Type string `json:"activity_type"`
					By   struct {
						Uuid string `json:"user_uuid"`
					} `json:"activity_by"`
				} `json:"task_activities"`
			} `json:"q"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(task_uuid, "`+task+`")) { task_activities { activity_type activity_by { user_uuid } } } }`), &out)
		var by []string
		for _, q := range out.Q {
			for _, a := range q.Activities {
				if a.Type == dgraphStruct.ACTIVITY_TYPE_STATUS {
					by = append(by, a.By.Uuid)
				}
			}
		}
		return by
	}
	// bot is the integration's bot, made the first time it acts.
	bot := func(t *testing.T) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`SELECT id FROM users WHERE email_id = $1`, userDomain.IntegrationBotEmail("github")).Scan(&id); err != nil {
			t.Fatalf("the integration's bot: %v", err)
		}
		return id
	}

	t.Run("a refused delivery is never claimed and moves nothing", func(t *testing.T) {
		if code := deliver("push", "d-unsigned", "", push("main", "Fixes WEB-2")); code != http.StatusUnauthorized {
			t.Errorf("unsigned: %d", code)
		}
		if code := deliver("push", "d-bad", "guessed", push("main", "Fixes WEB-2")); code != http.StatusUnauthorized {
			t.Errorf("badly signed: %d", code)
		}
		if code := deliver("push", "", "web-secret", push("main", "Fixes WEB-2")); code != http.StatusBadRequest {
			t.Errorf("no delivery id: %d", code)
		}
		var n int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM github_webhook_deliveries WHERE delivery_id IN ('d-unsigned', 'd-bad', '')`).Scan(&n)
		if n != 0 {
			t.Errorf("%d refused deliveries were claimed", n)
		}
		time.Sleep(time.Second)
		if s := status(t, w.other); s != "todo" || lines(t, w.other, "") != 0 {
			t.Errorf("a refused delivery touched WEB-2: %q", s)
		}
	})

	t.Run("a commit on the default branch closes its project's readable tasks, once", func(t *testing.T) {
		message := "Fixes WEB-1, BIL-1 and WEB-3"
		// A replay arriving while the first delivery still runs, and one after.
		if code := deliver("push", "d-1", "web-secret", push("main", message)); code != http.StatusOK {
			t.Fatalf("push: %d", code)
		}
		if code := deliver("push", "d-1", "web-secret", push("main", message)); code != http.StatusOK {
			t.Errorf("replay: %d", code)
		}
		handled(t, "d-1")
		if code := deliver("push", "d-1", "web-secret", push("main", message)); code != http.StatusOK {
			t.Errorf("late replay: %d", code)
		}
		time.Sleep(time.Second)
		if s := status(t, w.task); s != "done" {
			t.Errorf("WEB-1 is %q, want done", s)
		}
		if n := lines(t, w.task, "commit_linked"); n != 1 {
			t.Errorf("WEB-1's history has the commit %d times, want once", n)
		}
		if by := changedBy(t, w.task); len(by) != 1 || by[0] != bot(t) {
			t.Errorf("WEB-1's status changes were by %v, want once by the GitHub bot %s", by, bot(t))
		}
		if n := lines(t, theirs, ""); n != 0 {
			t.Errorf("Billing's BIL-1 took %d lines from acme/web", n)
		}
		if s := status(t, w.third); s != "todo" || lines(t, w.third, "") != 0 {
			t.Errorf("restricted WEB-3 was touched: %q", s)
		}
	})

	t.Run("a commit elsewhere links its task; the pull request's merge closes it", func(t *testing.T) {
		if code := deliver("push", "d-2", "web-secret", push("feature/web-2-export", "fixes WEB-2")); code != http.StatusOK {
			t.Fatalf("push: %d", code)
		}
		handled(t, "d-2")
		if s := status(t, w.other); s != "todo" {
			t.Errorf("a commit off the default branch moved WEB-2 to %q", s)
		}
		if lines(t, w.other, "commit_linked") != 1 || lines(t, w.other, "commit_pushed") != 1 {
			t.Errorf("WEB-2's history doesn't show the commit and the push to its branch")
		}
		merged := map[string]any{"action": "closed", "pull_request": map[string]any{
			"number": 31, "title": "Export the ledger", "html_url": "https://github.com/acme/web/pull/31", "state": "closed", "merged": true,
			"body": "Closes web-2", "user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": "feature/web-2-export"},
			"updated_at": time.Now().UTC().Format(time.RFC3339)}}
		if code := deliver("pull_request", "d-3", "web-secret", merged); code != http.StatusOK {
			t.Fatalf("merge: %d", code)
		}
		handled(t, "d-3")
		if s := status(t, w.other); s != "done" {
			t.Errorf("WEB-2 is %q after its pull request merged, want done", s)
		}
		// The link's merge rule moves WEB-2 and its closing words then find it
		// done. GitHub moved it, not the person who linked the repository, so
		// its history says the bot did, once.
		if by := changedBy(t, w.other); len(by) != 1 || by[0] != bot(t) {
			t.Errorf("WEB-2's status changes were by %v, want once by the GitHub bot %s", by, bot(t))
		}
	})

	// A pull request's edits rewrite only a task the pull request made: a key
	// in any pull request's words reaches a person's task, and its author's
	// later edits must not overwrite that task's name and description.
	t.Run("a pull request's edits stay off a task it names and reach the task it made", func(t *testing.T) {
		words := func(t *testing.T, task string) (name, description string) {
			t.Helper()
			var out struct {
				Q []struct {
					Name        string `json:"task_name"`
					Description string `json:"task_description"`
				} `json:"q"`
			}
			_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(task_uuid, "`+task+`")) { task_name task_description } }`), &out)
			if len(out.Q) != 1 {
				t.Fatalf("task %s isn't in the graph", task)
			}
			return out.Q[0].Name, out.Q[0].Description
		}
		// Edits come later than the sync that attached the pull request.
		later := time.Now().Add(time.Minute)

		// WEB-1, "Fix login", is a person's task; a pull request names it.
		if code := deliver("pull_request", "d-4", "web-secret", pr("opened", 41, "Speed up sign-in", "Fixes WEB-1", "speed-up-sign-in", time.Now())); code != http.StatusOK {
			t.Fatalf("opened: %d", code)
		}
		handled(t, "d-4")
		if _, url := w.links(w.task); url != "https://github.com/acme/web/pull/41" {
			t.Fatalf("the pull request naming WEB-1 didn't attach to it: %q", url)
		}
		// The link's pr_opened rule moves WEB-1 (closed by the push above) as
		// the bot too: GitHub opened the pull request.
		if s, by := status(t, w.task), changedBy(t, w.task); s != "inProgress" || len(by) != 2 || by[0] != bot(t) || by[1] != bot(t) {
			t.Errorf("WEB-1 is %q, its status changes by %v; want inProgress, both by the GitHub bot %s", s, by, bot(t))
		}
		if code := deliver("pull_request", "d-5", "web-secret", pr("edited", 41, "Speed up sign-in and cache it", "Fixes WEB-1 and caches the session", "speed-up-sign-in", later)); code != http.StatusOK {
			t.Fatalf("edited: %d", code)
		}
		handled(t, "d-5")
		if name, description := words(t, w.task); name != "Fix login" || description != "" {
			t.Errorf("the pull request's edit rewrote WEB-1 to %q / %q", name, description)
		}

		// A pull request naming no task makes one in Website (in its team),
		// and that task follows the pull request's edits.
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
		if code := deliver("pull_request", "d-6", "web-secret", pr("opened", 42, "Tidy the footer", "Moves the links", "tidy-footer", time.Now())); code != http.StatusOK {
			t.Fatalf("opened: %d", code)
		}
		handled(t, "d-6")
		var made string
		if err := env.PG.QueryRow(`SELECT id FROM tasks WHERE github_pr_url = 'https://github.com/acme/web/pull/42' AND project_id = $1`, w.project).Scan(&made); err != nil {
			t.Fatalf("the pull request naming no task made none: %v", err)
		}
		if code := deliver("pull_request", "d-7", "web-secret", pr("edited", 42, "Tidy the footer and header", "Moves the links and the logo", "tidy-footer", later)); code != http.StatusOK {
			t.Fatalf("edited: %d", code)
		}
		handled(t, "d-7")
		if name, description := words(t, made); name != "Tidy the footer and header" || description != "Moves the links and the logo" {
			t.Errorf("the task the pull request made reads %q / %q, want its edited title and body", name, description)
		}
	})

	// A ref reaches only a task the integration's bot may read, and a task's
	// stored branch is a ref like its key. WEB-3 is restricted to people the
	// bot isn't and carries the branch a pull request opens from: that pull
	// request names no task, so it makes its own in Website, which carries
	// the branch too and follows the pull request to its merge. WEB-3 is
	// neither attached nor moved, and nothing names it: no github.pr.opened
	// (an AI review would comment on it) and no message on Website's topic.
	t.Run("a restricted task's stored branch is no lead: its pull request neither attaches to it nor moves it, and nothing about it goes out", func(t *testing.T) {
		var mu sync.Mutex
		var reviewed []string
		webhookBusiness.RegisterEventListenerSeeingRestricted(func(_ context.Context, kind string, data map[string]interface{}) {
			if task, _ := data["task_uuid"].(string); kind == "github.pr.opened" {
				mu.Lock()
				reviewed = append(reviewed, task)
				mu.Unlock()
			}
		})
		asked := func(task string) bool {
			mu.Lock()
			defer mu.Unlock()
			for _, r := range reviewed {
				if r == task {
					return true
				}
			}
			return false
		}
		sent := len(mq.All())
		history := lines(t, w.third, "")

		w.exec(`UPDATE tasks SET github_branch = 'cache-sessions' WHERE id = $1`, w.third)
		if code := deliver("pull_request", "d-8", "web-secret", pr("opened", 43, "Cache sessions", "", "cache-sessions", time.Now())); code != http.StatusOK {
			t.Fatalf("opened: %d", code)
		}
		handled(t, "d-8")
		if _, url := w.links(w.third); url != "" {
			t.Errorf("the pull request opened from restricted WEB-3's branch attached to it (%q)", url)
		}
		var made string
		if err := env.PG.QueryRow(`SELECT id FROM tasks WHERE github_pr_url = 'https://github.com/acme/web/pull/43' AND project_id = $1 AND id <> $2`, w.project, w.third).Scan(&made); err != nil {
			t.Fatalf("the pull request from WEB-3's branch made no task of its own in Website: %v", err)
		}
		// The task it made is reviewed, as any pull request's is; WEB-3 never is.
		for deadline := time.Now().Add(10 * time.Second); !asked(made) && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		}
		if !asked(made) || asked(w.third) {
			t.Errorf("github.pr.opened went out for %v; want the task the pull request made (%s), never WEB-3", reviewed, made)
		}

		// Its merge reaches the task it made, which carries WEB-3's branch.
		merged := pr("closed", 43, "Cache sessions", "", "cache-sessions", time.Now().Add(time.Minute))
		merged["pull_request"].(map[string]any)["merged"] = true
		if code := deliver("pull_request", "d-9", "web-secret", merged); code != http.StatusOK {
			t.Fatalf("merge: %d", code)
		}
		handled(t, "d-9")
		if s := status(t, made); s != "done" || lines(t, made, "pr_merged") != 1 {
			t.Errorf("the task the pull request made is %q with %d merge lines after it merged; want done, one", s, lines(t, made, "pr_merged"))
		}
		if s, by := status(t, w.third), changedBy(t, w.third); s != "todo" || len(by) != 0 {
			t.Errorf("WEB-3 is %q, its status changes by %v; want todo with none", s, by)
		}
		if n := lines(t, w.third, ""); n != history {
			t.Errorf("WEB-3's history went from %d GitHub lines to %d", history, n)
		}
		// Website's topic is told of the task the pull request made, never of WEB-3.
		var toldMade bool
		for _, p := range mq.All()[sent:] {
			if bytes.Contains(p.Payload, []byte(w.third)) {
				t.Errorf("%s was told of WEB-3: %s", p.Topic, p.Payload)
			}
			toldMade = toldMade || bytes.Contains(p.Payload, []byte(made))
		}
		if !toldMade {
			t.Errorf("no message named the task the pull request made (%s)", made)
		}
	})
}

// A delivery GitHub sends again is run again only when its last run failed,
// or when its run stopped long ago (a server that went away mid-run left it
// 'processing' for over 5 minutes). One still running is a replay: skipped.
func TestAGitHubDeliveryIsClaimedAgainOnlyAfterItFailedOrStalled(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	// seen is a delivery already claimed once, in status, last touched ago.
	seen := func(t *testing.T, delivery, status string, ago time.Duration) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO github_webhook_deliveries (delivery_id, event_type, status, attempts, received_at, updated_at)
			VALUES ($1, 'push', $2, 1, NOW() - make_interval(secs => $3::float8), NOW() - make_interval(secs => $3::float8))`, delivery, status, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, delivery string) bool {
		t.Helper()
		ok, err := githubWebhookDeliveryDomain.ClaimGitHubWebhookDelivery(ctx, delivery, "push")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	row := func(t *testing.T, delivery string) (status string, attempts int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT status, attempts FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		return status, attempts
	}

	t.Run("a delivery whose run failed is claimed again", func(t *testing.T) {
		seen(t, "d-failed", "failed", time.Minute)
		if !claim(t, "d-failed") {
			t.Fatal("a failed delivery sent again wasn't claimed")
		}
		if status, attempts := row(t, "d-failed"); status != "processing" || attempts != 2 {
			t.Errorf("the failed delivery is %s after %d attempts, want processing after 2", status, attempts)
		}
	})
	t.Run("a delivery processing for 6 minutes is claimed again", func(t *testing.T) {
		seen(t, "d-stalled", "processing", 6*time.Minute)
		if !claim(t, "d-stalled") {
			t.Fatal("a delivery whose run stopped 6 minutes ago wasn't claimed")
		}
		if status, attempts := row(t, "d-stalled"); status != "processing" || attempts != 2 {
			t.Errorf("the stalled delivery is %s after %d attempts, want processing after 2", status, attempts)
		}
	})
	t.Run("a delivery processing for a minute is not", func(t *testing.T) {
		seen(t, "d-running", "processing", time.Minute)
		if claim(t, "d-running") {
			t.Fatal("a delivery still running was claimed again")
		}
		if status, attempts := row(t, "d-running"); status != "processing" || attempts != 1 {
			t.Errorf("the running delivery is %s after %d attempts, want processing after 1", status, attempts)
		}
	})
}
