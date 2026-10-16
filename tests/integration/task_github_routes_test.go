//go:build integration
// +build integration

package integration_test

// A task's GitHub side is its project's: the task routes check the task like
// every other task route, links go only to the project's own repositories,
// the sync never posts a task anywhere else, and links made outside them
// before this are undone at start (PROJ-BUG-4).
//
// Eleven of the twelve /task/github-* routes checked nothing but sign-in.
// Anyone could point a private task at any issue (its later comments were
// then posted there with the workspace's GitHub token), unlink, branch, open
// pull requests and search a linked private repository.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTaskGitHubRoutesBelongToTheProject -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	syncBusiness "github.com/akashc777/OneCamp/business"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTaskGitHubRoutesBelongToTheProject(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	gh := recordGitHub(t)
	w := seedGitHubWorkspace(t, env, dg)

	type route struct {
		name    string
		handler http.HandlerFunc
		method  string
		query   string
		body    any
		change  bool
	}
	issue := "https://github.com/acme/web/issues/5"
	single := []route{
		{"link", taskController.LinkTaskToGitHub, http.MethodPost, "", map[string]string{"url": issue}, true},
		{"unlink", taskController.UnlinkTaskFromGitHub, http.MethodPost, "", nil, true},
		{"branch", taskController.CreateGitHubBranchForTask, http.MethodPost, "", map[string]string{"branch_name": "fix-login"}, true},
		{"retry", taskController.RetryGitHubSync, http.MethodPost, "", nil, true},
		{"refresh", taskController.RefreshFromGitHub, http.MethodPost, "", nil, true},
		{"pull request", taskController.CreateGitHubPRForTask, http.MethodPost, "", map[string]string{"title": "Fix login"}, true},
		{"activity", taskController.GetGitHubTaskActivity, http.MethodGet, "", nil, false},
		{"reviews", taskController.GetGitHubPRReviews, http.MethodGet, "", nil, false},
		{"sync status", taskController.GetGitHubSyncStatus, http.MethodGet, "", nil, false},
		{"issue search", taskController.SearchGitHubIssues, http.MethodGet, "?q=login", nil, false},
		{"pull request search", taskController.SearchGitHubPRs, http.MethodGet, "?q=login", nil, false},
	}
	call := func(who string, rt route, task string) (int, string) {
		t.Helper()
		var body io.Reader = http.NoBody
		if rt.body != nil {
			raw, _ := json.Marshal(rt.body)
			body = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("task_uuid", task)
		rec := httptest.NewRecorder()
		rt.handler(rec, httptest.NewRequest(rt.method, "/"+rt.query, body).WithContext(context.WithValue(w.as(who), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	bulk := func(t *testing.T, who string, handler http.HandlerFunc, payload any) (succeeded int, refusals []string) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(w.as(who)))
		var out struct {
			Success int      `json:"success_count"`
			Errors  []string `json:"errors"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("bulk change as %s: %d %s", who, rec.Code, rec.Body.String())
		}
		return out.Success, out.Errors
	}
	link := func(task string, address string) any {
		return map[string]any{"links": []map[string]any{{"task_uuid": task, "issue_number": 5, "issue_url": address}}}
	}
	unlinked := func(t *testing.T, task string) {
		t.Helper()
		if issue, pr := w.links(task); issue != "" || pr != "" {
			t.Fatalf("the task was linked to %q / %q", issue, pr)
		}
	}

	t.Run("someone outside the project finds no task behind any route", func(t *testing.T) {
		for _, rt := range single {
			if code, body := call("cy", rt, w.task); code != http.StatusNotFound {
				t.Errorf("%s as someone outside the project: %d %s", rt.name, code, body)
			}
		}
		for name, try := range map[string]func() (int, []string){
			"bulk link": func() (int, []string) {
				return bulk(t, "cy", taskController.BulkLinkTasksToGitHub, link(w.task, issue))
			},
			"bulk unlink": func() (int, []string) {
				return bulk(t, "cy", taskController.BulkUnlinkTasksFromGitHub, map[string]any{"task_uuids": []string{w.task}})
			},
		} {
			if n, refusals := try(); n != 0 || len(refusals) != 1 || !strings.HasPrefix(refusals[0], "Task not found") {
				t.Errorf("%s as someone outside the project: %d done, %v", name, n, refusals)
			}
		}
		unlinked(t, w.task)
		if gh.count("") != 0 {
			t.Errorf("GitHub was called for someone outside the project: %v", gh.calls())
		}
	})

	t.Run("a member who isn't an admin reads but changes nothing", func(t *testing.T) {
		for _, rt := range single {
			code, body := call("ben", rt, w.task)
			switch {
			case rt.change && code != http.StatusForbidden:
				t.Errorf("%s as a member: %d %s, want it refused", rt.name, code, body)
			case !rt.change && (code == http.StatusNotFound || code == http.StatusForbidden):
				t.Errorf("%s as a member: %d %s, want it read", rt.name, code, body)
			}
		}
		if n, refusals := bulk(t, "ben", taskController.BulkLinkTasksToGitHub, link(w.task, issue)); n != 0 || len(refusals) != 1 ||
			!strings.HasPrefix(refusals[0], "Only the project's admins") {
			t.Errorf("bulk link as a member: %d done, %v", n, refusals)
		}
		unlinked(t, w.task)
		for _, c := range gh.calls() {
			if !strings.HasPrefix(c, "GET /search/issues") {
				t.Errorf("a member's request reached GitHub with %s", c)
			}
		}
	})

	t.Run("links go only to the project's own repositories", func(t *testing.T) {
		if code, body := call("ana", single[0], w.task); code != http.StatusOK {
			t.Fatalf("an admin linking an issue in the project's repository: %d %s", code, body)
		}
		elsewhere := route{"link", taskController.LinkTaskToGitHub, http.MethodPost, "", map[string]string{"url": "https://github.com/someone/elsewhere/issues/1"}, true}
		if code, _ := call("ana", elsewhere, w.other); code != http.StatusBadRequest {
			t.Errorf("an admin linking a task to a repository the project hasn't linked: %d", code)
		}
		// An issue moved out of the project's repository: GitHub answers with
		// where it is now, which is what would be stored.
		gh.move("/repos/acme/web/issues/41", "https://github.com/someone/elsewhere/issues/2")
		moved := route{"link", taskController.LinkTaskToGitHub, http.MethodPost, "", map[string]string{"url": "https://github.com/acme/web/issues/41"}, true}
		if code, body := call("ana", moved, w.other); code != http.StatusBadRequest {
			t.Errorf("an admin linking an issue that moved out of the project's repository: %d %s", code, body)
		}
		unlinked(t, w.other)
		if n, refusals := bulk(t, "ana", taskController.BulkLinkTasksToGitHub, link(w.other, "https://github.com/someone/elsewhere/issues/1")); n != 0 || len(refusals) != 1 {
			t.Errorf("bulk link to a repository the project hasn't linked: %d done, %v", n, refusals)
		}
		unlinked(t, w.other)
		if n, refusals := bulk(t, "ana", taskController.BulkLinkTasksToGitHub, link(w.other, "https://github.com/acme/web/issues/6")); n != 1 {
			t.Errorf("bulk link to the project's repository: %d done, %v", n, refusals)
		}
		if got, _ := w.links(w.other); got != "https://github.com/acme/web/issues/6" {
			t.Errorf("bulk link stored %q", got)
		}
		if n, refusals := bulk(t, "ana", taskController.BulkUnlinkTasksFromGitHub, map[string]any{"task_uuids": []string{w.other}}); n != 1 {
			t.Errorf("bulk unlink as an admin: %d done, %v", n, refusals)
		}
		unlinked(t, w.other)
	})

	t.Run("unlinking takes the pull request off too", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/acme/web/pull/8', github_pr_number = 8 WHERE id = $1`, w.other)
		if code, body := call("ana", single[1], w.other); code != http.StatusOK {
			t.Fatalf("unlink: %d %s", code, body)
		}
		unlinked(t, w.other)
	})

	t.Run("the sync posts a task only to its project's repositories", func(t *testing.T) {
		// A link from before the guards, to a repository the project never linked.
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/someone/elsewhere/issues/9', github_issue_number = 9 WHERE id = $1`, w.other)
		gh.reset()
		workers, stopWorkers := context.WithCancel(ctx)
		defer stopWorkers()
		githubBusiness.StartGitHubSyncWorker(syncBusiness.SyncSignal, workers)
		for _, task := range []string{w.task, w.other} {
			syncBusiness.EnqueueGitHubSync(ctx, uuid.MustParse(task), "comment", map[string]interface{}{
				"comment_body": "Severance terms are in the drive", "comment_uuid": uuid.NewString()})
		}
		deadline := time.Now().Add(60 * time.Second)
		for gh.count("POST /repos/acme/web/issues/5/comments") == 0 || w.queued(w.other) != "failed" {
			if time.Now().After(deadline) {
				t.Fatalf("the sync didn't finish: GitHub saw %v; the other task's item is %q", gh.calls(), w.queued(w.other))
			}
			time.Sleep(200 * time.Millisecond)
		}
		if n := gh.count("/repos/someone/"); n != 0 {
			t.Errorf("the task linked outside its project was posted there: %v", gh.calls())
		}
		var status, reason string
		if err := env.PG.QueryRow(`SELECT github_sync_status, COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, w.other).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !strings.Contains(reason, "isn't linked to") {
			t.Errorf("the refused task's sync reads %q %q", status, reason)
		}
	})

	t.Run("links made outside the project before are undone at start", func(t *testing.T) {
		// One pull request that attached itself from another project's repository.
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/acme/billing/pull/3', github_pr_number = 3 WHERE id = $1`, w.third)
		undone, err := githubBusiness.RepairTaskLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, row := range undone {
			got[row.TaskID.String()] = true
		}
		if len(got) != 2 || !got[w.other] || !got[w.third] {
			t.Errorf("unlinked %v, want the two linked outside their project", undone)
		}
		unlinked(t, w.other)
		unlinked(t, w.third)
		if issue, _ := w.links(w.task); issue != "https://github.com/acme/web/issues/5" {
			t.Errorf("the task linked in its own project lost its link (now %q)", issue)
		}
		if again, err := githubBusiness.RepairTaskLinks(ctx); err != nil || len(again) != 0 {
			t.Errorf("a second start unlinked %v (%v)", again, err)
		}
	})

	t.Run("unlinking a repository unlinks only the tasks linked into it", func(t *testing.T) {
		// Website links a second repository, acme/docs, with a task linked to
		// an issue there; another task is linked to acme/web by a pull request
		// alone. Unlinking acme/web once cleared every issue-linked task of the
		// project, whatever its repository, and left that pull request linked.
		w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, created_by) VALUES ($1, 'acme', 'docs', true, true, $2)`, w.project, w.people["ana"])
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/docs/issues/2', github_issue_number = 2 WHERE id = $1`, w.other)
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/acme/web/pull/9', github_pr_number = 9 WHERE id = $1`, w.third)
		var web uuid.UUID
		if err := env.PG.QueryRow(`SELECT id FROM github_links WHERE project_id = $1 AND repo_name = 'web' AND deleted_at IS NULL`, w.project).Scan(&web); err != nil {
			t.Fatal(err)
		}
		if err := githubBusiness.UnlinkRepository(ctx, web); err != nil {
			t.Fatal(err)
		}
		unlinked(t, w.task)
		unlinked(t, w.third)
		if issue, _ := w.links(w.other); issue != "https://github.com/acme/docs/issues/2" {
			t.Errorf("the task linked to the project's other repository lost its link (now %q)", issue)
		}
	})

	// Deleting a task sends its issue the task's last change, closing it as
	// not planned. The guard on what a sync may send read the project of live
	// tasks alone, so it refused a deleted one as outside its project: the
	// issue stayed open, and the task's sync read failed.
	t.Run("deleting a linked task closes its issue", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/docs/issues/4', github_issue_number = 4,
			github_sync_status = NULL, github_sync_error = NULL WHERE id = $1`, w.task)
		gh.reset()
		workers, stopWorkers := context.WithCancel(ctx)
		defer stopWorkers()
		githubBusiness.StartGitHubSyncWorker(syncBusiness.SyncSignal, workers)
		// What deleting it does here (ArchiveTaskByTaskUUID): the task is
		// deleted, then its last change goes to GitHub.
		w.exec(`UPDATE tasks SET deleted_at = NOW() WHERE id = $1`, w.task)
		syncBusiness.EnqueueGitHubSync(ctx, uuid.MustParse(w.task), "status", map[string]interface{}{"status": "canceled"})
		deadline := time.Now().Add(60 * time.Second)
		for gh.count("PATCH /repos/acme/docs/issues/4") == 0 {
			if time.Now().After(deadline) {
				var status, reason string
				_ = env.PG.QueryRow(`SELECT COALESCE(github_sync_status, ''), COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, w.task).Scan(&status, &reason)
				t.Fatalf("the deleted task's issue wasn't closed: GitHub saw %v; the task's sync reads %q %q", gh.calls(), status, reason)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	// Last: it takes the graph away. A failed read was answered as "not
	// found", telling an admin their task or project was gone.
	t.Run("a read that fails is a failure, never not found", func(t *testing.T) {
		asAna, asCy := w.as("ana"), w.as("cy")
		with := func(as context.Context, key, value string) *http.Request {
			rc := chi.NewRouteContext()
			rc.URLParams.Add(key, value)
			return httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(as, chi.RouteCtxKey, rc))
		}
		rec := httptest.NewRecorder()
		if _, _, ok := projectaccess.Require(rec, with(asCy, "project_uuid", w.project), false, ""); ok || rec.Code != http.StatusNotFound {
			t.Errorf("a project's route for someone outside it: %d %s, want 404", rec.Code, rec.Body.String())
		}

		dg.Break(t)
		rec = httptest.NewRecorder()
		taskController.GetGitHubTaskActivity(rec, with(asAna, "task_uuid", w.other))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("a task's route while the task can't be read: %d %s, want 503", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		if _, _, ok := projectaccess.Require(rec, with(asAna, "project_uuid", w.project), false, ""); ok || rec.Code != http.StatusServiceUnavailable {
			t.Errorf("a project's route while the project can't be read: %d %s, want 503", rec.Code, rec.Body.String())
		}
	})
}

// githubWorkspace is a project ("Website") with an admin (ana) and a member
// (ben), someone outside it (cy), three of its tasks, its repository acme/web,
// and another project ("Billing") with acme/billing; GitHub is connected.
type githubWorkspace struct {
	t                  *testing.T
	db                 *integration.Env
	people             map[string]uuid.UUID
	project, billing   string
	task, other, third string
}

func seedGitHubWorkspace(t *testing.T, env *integration.Env, dg *integration.DgraphEnv) *githubWorkspace {
	t.Helper()
	w := &githubWorkspace{t: t, db: env, people: map[string]uuid.UUID{}}
	nodes := []map[string]any{}
	for _, name := range []string{"ana", "ben", "cy"} {
		id := uuid.New()
		w.people[name] = id
		w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	w.project, w.billing = uuid.NewString(), uuid.NewString()
	w.task, w.other, w.third = uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $3), ($2, 'Billing', $3)`, w.project, w.billing, w.people["ana"])
	for _, task := range []string{w.task, w.other, w.third} {
		w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, w.project, w.people["ana"])
	}
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, created_by)
		VALUES ($1, 'acme', 'web', true, true, $3), ($2, 'acme', 'billing', true, true, $3)`, w.project, w.billing, w.people["ana"])
	w.exec(`INSERT INTO integrations (entity_type, entity_id, provider, access_token) VALUES ('org', $1, 'github', 'test-token')`, uuid.Nil)

	live := "0001-01-01T00:00:00Z"
	tasks := []map[string]any{}
	for i, task := range []string{w.task, w.other, w.third} {
		tasks = append(tasks, map[string]any{"dgraph.type": "Task", "task_uuid": task, "task_name": []string{"Fix login", "Severance letters", "Billing export"}[i],
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ana"}})
	}
	nodes = append(nodes, map[string]any{"uid": "_:website", "dgraph.type": "Project", "project_uuid": w.project, "project_name": "Website",
		"project_deleted_at": live, "project_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}},
		"project_admins": []map[string]any{{"uid": "_:ana"}}, "project_created_by": map[string]any{"uid": "_:ana"}, "project_tasks": tasks})
	dg.Mutate(t, nodes)
	return w
}

func (w *githubWorkspace) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.db.PG.Exec(q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// as is a request context signed in as the person.
func (w *githubWorkspace) as(name string) context.Context {
	w.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		w.t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		w.t.Fatalf("load %s's node: %v", name, err)
	}
	return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// links is the task's stored issue and pull request addresses.
func (w *githubWorkspace) links(task string) (issue, pr string) {
	w.t.Helper()
	if err := w.db.PG.QueryRow(`SELECT COALESCE(github_issue_url, ''), COALESCE(github_pr_url, '') FROM tasks WHERE id = $1`, task).Scan(&issue, &pr); err != nil {
		w.t.Fatal(err)
	}
	return issue, pr
}

// queued is the state of the task's latest sync item, "" for none.
func (w *githubWorkspace) queued(task string) (state string) {
	w.t.Helper()
	_ = w.db.PG.QueryRow(`SELECT status FROM github_sync_queue WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`, task).Scan(&state)
	return state
}

// githubRecorder stands in for GitHub's REST API until the test ends: it
// answers every request with an empty success (a new comment gets an id) and
// keeps "METHOD /path" of each. The sync and the handlers call through the
// default transport. An issue moved to another repository (move) answers
// with its new address, as GitHub does.
type githubRecorder struct {
	real  http.RoundTripper
	mu    sync.Mutex
	seen  []string
	moved map[string]string
}

func recordGitHub(t *testing.T) *githubRecorder {
	t.Helper()
	g := &githubRecorder{real: http.DefaultTransport, moved: map[string]string{}}
	http.DefaultTransport = g
	t.Cleanup(func() { http.DefaultTransport = g.real })
	return g
}

func (g *githubRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return g.real.RoundTrip(r)
	}
	g.mu.Lock()
	g.seen = append(g.seen, r.Method+" "+r.URL.Path)
	address, moved := g.moved[r.URL.Path]
	g.mu.Unlock()
	if !moved {
		address = "https://github.com" + strings.Replace(r.URL.Path, "/repos", "", 1)
	}
	body := `{"id": 77, "number": 5, "html_url": "` + address + `", "object": {"sha": "abc"}}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// move answers GitHub's API path with the address an issue has moved to.
func (g *githubRecorder) move(path, address string) {
	g.mu.Lock()
	g.moved[path] = address
	g.mu.Unlock()
}

func (g *githubRecorder) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

// count is how many requests GitHub saw whose "METHOD /path" contains part.
func (g *githubRecorder) count(part string) int {
	n := 0
	for _, c := range g.calls() {
		if strings.Contains(c, part) {
			n++
		}
	}
	return n
}

func (g *githubRecorder) reset() {
	g.mu.Lock()
	g.seen = nil
	g.mu.Unlock()
}
