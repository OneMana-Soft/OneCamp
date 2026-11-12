//go:build integration
// +build integration

package integration_test

// The workspace's GitHub connection is the token of the admin who connected
// it: "repo" scope on every organisation that admin is in. What a member does
// through it stays inside the repositories their project links (SEC1, 9 Nov):
//
//   - a search sends GitHub the member's words, and GitHub reads a qualifier
//     in them (repo:, org:, user:, is:, ...) as part of the search: two repo:
//     qualifiers are either repository, so "x repo:someone/private" listed
//     another repository's issues and pull requests, and a stray quote or NOT
//     undid the scope altogether;
//   - a file path with dot segments ("../../../someone/private/contents/f")
//     names another repository's file once a server resolves it;
//   - and on the public demo, which holds a real connection, nothing is
//     written to GitHub at all: by a route, by the sync, or by a start
//     registering a repository's webhook again.
//
// Against real Postgres and Dgraph, with GitHub's API stood in and every
// request to it kept.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGitHubReachStaysInLinkedRepositories -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	syncBusiness "github.com/akashc777/OneCamp/business"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Qualifiers GitHub documents for searching issues and pull requests, and
// code; each is tried in a member's words, bare, negated and quoted.
var githubQualifiers = []string{
	"repo", "org", "user", "owner", "is", "in", "type", "state", "reason", "author", "assignee",
	"mentions", "commenter", "involves", "team", "label", "milestone", "project", "status", "head",
	"base", "language", "comments", "interactions", "reactions", "draft", "review", "reviewed-by",
	"review-requested", "user-review-requested", "team-review-requested", "created", "updated",
	"closed", "merged", "archived", "no", "linked", "sort", "path", "size", "filename",
	"extension", "fork", "symbol", "content", "enterprise",
}

func TestGitHubReachStaysInLinkedRepositories(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	gh := recordGitHub(t)
	// Website (ana its admin, ben a member) links acme/web; Billing links
	// acme/billing; cy is in neither.
	w := seedGitHubWorkspace(t, env, dg)
	joinProject(t, dg, w.project, w.people["ana"], w.people["ben"])

	call := func(who string, handler http.HandlerFunc, method, query string, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("task_uuid", w.task)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(method, "/"+query, bytes.NewReader(raw)).WithContext(context.WithValue(w.as(who), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	// scopedTo fails unless q, a search GitHub was sent, names the one
	// repository and kind it was asked for, and nothing a member's words put
	// there: no other qualifier, quote, operator or bracket.
	scopedTo := func(t *testing.T, what, q, repo string, kinds ...string) {
		t.Helper()
		allowed := map[string]bool{"repo:" + repo: true}
		for _, k := range kinds {
			allowed[k] = true
		}
		repos := 0
		for _, word := range strings.Fields(q) {
			if strings.HasPrefix(strings.ToLower(word), "repo:") {
				repos++
			}
			switch {
			case strings.Contains(word, ":") && !allowed[word]:
				t.Errorf("%s: GitHub was asked %q, with the qualifier %q", what, q, word)
			case strings.ContainsAny(word, `"()\`):
				t.Errorf("%s: GitHub was asked %q, with %q", what, q, word)
			case word == "AND" || word == "OR" || word == "NOT":
				t.Errorf("%s: GitHub was asked %q, with the operator %s", what, q, word)
			}
		}
		if repos != 1 || !strings.Contains(" "+q+" ", " repo:"+repo+" ") {
			t.Errorf("%s: GitHub was asked %q, want it held to repo:%s alone", what, q, repo)
		}
		for _, k := range kinds {
			if !strings.Contains(" "+q+" ", " "+k+" ") {
				t.Errorf("%s: GitHub was asked %q, without %s", what, q, k)
			}
		}
	}
	tries := []string{
		`login repo:someone/private`,
		`login "repo:someone/private"`,
		`"login repo:someone/private`,
		`login NOT`,
		`login OR (repo:someone/private)`,
		`login -repo:acme/web secret`,
		`login is:pr is:issue`,
		`login\ repo:someone/private`,
		`login REPO:someone/private`,
	}
	for _, q := range githubQualifiers {
		tries = append(tries, "login "+q+":someone", "login -"+q+":someone", `login `+q+`:"someone else"`)
	}

	t.Run("a member's search names only their project's repository", func(t *testing.T) {
		for _, route := range []struct {
			name    string
			handler http.HandlerFunc
			kind    string
		}{
			{"issue search", taskController.SearchGitHubIssues, "is:issue"},
			{"pull request search", taskController.SearchGitHubPRs, "is:pr"},
		} {
			for _, who := range []string{"ana", "ben"} {
				for _, q := range tries {
					gh.reset()
					code, body := call(who, route.handler, http.MethodGet, "?q="+url.QueryEscape(q), nil)
					if code != http.StatusOK {
						t.Fatalf("%s as %s for %q: %d %s", route.name, who, q, code, body)
					}
					scopedTo(t, route.name+" for "+q, gh.searched("GET /search/issues"), "acme/web", route.kind)
					if n := gh.count("someone"); n != 0 {
						t.Errorf("%s for %q reached another repository: %v", route.name, q, gh.calls())
					}
				}
			}
		}
	})

	aiBusiness.RegisterToolExecutors()
	t.Run("the AI's code search names only the repository it was allowed", func(t *testing.T) {
		search := ai.Executors["search_repo_code"]
		for _, q := range tries {
			gh.reset()
			if _, _, err := search(ctx, ai.ProposedAction{Params: map[string]string{"owner": "acme", "repo": "web", "query": q}}, w.people["ben"].String()); err != nil {
				t.Fatalf("ben searching his project's repository for %q: %v", q, err)
			}
			scopedTo(t, "code search for "+q, gh.searched("GET /search/code"), "acme/web")
		}
	})

	t.Run("a file is read only inside the repository it was allowed", func(t *testing.T) {
		read := ai.Executors["read_repo_file"]
		for _, path := range []string{
			"../../../someone/private/contents/secret.txt",
			"docs/../../../../someone/private/contents/secret.txt",
			"./../web2/contents/x",
			"src/..",
			"a//b",
		} {
			gh.reset()
			_, _, _ = read(ctx, ai.ProposedAction{Params: map[string]string{"owner": "acme", "repo": "web", "path": path}}, w.people["ben"].String())
			if seen := gh.calls(); len(seen) != 0 {
				t.Errorf("reading %q asked GitHub %v, want it refused before", path, seen)
			}
		}
		// A path that stays inside is read as it was.
		gh.reset()
		if _, _, err := read(ctx, ai.ProposedAction{Params: map[string]string{"owner": "acme", "repo": "web", "path": "/src/app/main.go"}}, w.people["ben"].String()); err != nil {
			t.Fatalf("ben reading a file of his project's repository: %v", err)
		}
		if seen := gh.calls(); len(seen) != 1 || seen[0] != "GET /repos/acme/web/contents/src/app/main.go" {
			t.Errorf("ben's read asked GitHub %v, want the one file", seen)
		}
	})

	// The demo holds a real GitHub connection (its webhook is registered at
	// each start), and everyone who opens it is the same visitor.
	t.Run("the demo writes nothing to GitHub", func(t *testing.T) {
		t.Setenv("DEMO_MODE", "true")
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/web/issues/5', github_issue_number = 5,
			github_sync_status = NULL, github_sync_error = NULL WHERE id = $1`, w.task)

		// The routes that write to GitHub say so first, in plain words, to
		// anyone: the demo's own team shares the connection too.
		for _, route := range []struct {
			name    string
			handler http.HandlerFunc
			body    any
		}{
			{"creating a branch", taskController.CreateGitHubBranchForTask, map[string]string{"branch_name": "fix-login"}},
			{"opening a pull request", taskController.CreateGitHubPRForTask, map[string]string{"title": "Fix login"}},
			{"sending the task again", taskController.RetryGitHubSync, nil},
		} {
			gh.reset()
			code, body := call("ana", customMiddleware.NoGitHubWritesInDemo(route.handler).ServeHTTP, http.MethodPost, "", route.body)
			var out struct{ Code, Msg string }
			_ = json.Unmarshal([]byte(body), &out)
			if code != http.StatusForbidden || out.Code != "demo" || !strings.HasPrefix(out.Msg, "The demo is shared by everyone who opens it") {
				t.Errorf("%s on the demo: %d %s, want 403 demo, in plain words", route.name, code, body)
			}
			if seen := gh.calls(); len(seen) != 0 {
				t.Errorf("%s on the demo asked GitHub %v", route.name, seen)
			}
		}

		// The workspace's own client: a branch made from the task reads the
		// default branch, and creates nothing.
		gh.reset()
		if code, body := call("ana", taskController.CreateGitHubBranchForTask, http.MethodPost, "", map[string]string{"branch_name": "fix-login"}); code == http.StatusOK {
			t.Errorf("creating a branch on the demo: %d %s, want it refused", code, body)
		}
		for _, c := range gh.calls() {
			if !strings.HasPrefix(c, "GET ") {
				t.Errorf("the demo sent GitHub %s", c)
			}
		}

		// The sync: a change to a linked task is kept in OneCamp, and the item
		// that would carry it to GitHub fails at once, saying why.
		gh.reset()
		workers, stopWorkers := context.WithCancel(ctx)
		defer stopWorkers()
		githubBusiness.StartGitHubSyncWorker(syncBusiness.SyncSignal, workers)
		syncBusiness.EnqueueGitHubSync(ctx, uuid.MustParse(w.task), "status", map[string]interface{}{"status": "done"})
		deadline := time.Now().Add(60 * time.Second)
		for state := w.queued(w.task); state != "failed" && state != "completed"; state = w.queued(w.task) {
			if time.Now().After(deadline) {
				t.Fatalf("the sync didn't finish: GitHub saw %v; the item is %q", gh.calls(), state)
			}
			time.Sleep(200 * time.Millisecond)
		}
		var attempts int
		_ = env.PG.QueryRow(`SELECT attempts FROM github_sync_queue WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`, w.task).Scan(&attempts)
		if state := w.queued(w.task); state != "failed" || attempts != 1 {
			t.Errorf("the demo's sync item is %q after %d tries, want it failed at the first: it's refused however often it's tried", state, attempts)
		}
		for _, c := range gh.calls() {
			if !strings.HasPrefix(c, "GET ") {
				t.Errorf("the demo's sync sent GitHub %s", c)
			}
		}
		var status, reason string
		if err := env.PG.QueryRow(`SELECT COALESCE(github_sync_status, ''), COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, w.task).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !strings.HasPrefix(reason, "The demo is shared by everyone who opens it") {
			t.Errorf("the demo task's sync reads %q %q, want it failed in plain words", status, reason)
		}

		// A start registers no repository's webhook again: that was the write
		// the demo made at every start, on a real repository. Elsewhere the
		// same links' webhooks are listed and signed again
		// (TestTaskGitHubRoutesBelongToTheProject).
		w.exec(`UPDATE github_links SET webhook_secret = 'demo-secret' WHERE deleted_at IS NULL`)
		gh.reset()
		githubBusiness.StartWebhookRefresh(ctx)
		integration.Settle(t)
		if seen := gh.calls(); len(seen) != 0 {
			t.Errorf("a start on the demo asked GitHub %v, want nothing", seen)
		}

		// Reads go on, held to the linked repository.
		gh.reset()
		if code, body := call("ben", taskController.SearchGitHubIssues, http.MethodGet, "?q=login", nil); code != http.StatusOK {
			t.Errorf("searching on the demo: %d %s", code, body)
		}
		if gh.count("GET /search/issues") != 1 {
			t.Errorf("the demo's search asked GitHub %v", gh.calls())
		}
	})
}
