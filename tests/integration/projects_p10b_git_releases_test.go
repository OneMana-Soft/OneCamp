//go:build integration
// +build integration

package integration_test

// Releases from Git (P10-1b job 2, jira-04 + jira-09): Website and Billing
// both link acme/shop (Archive, archived, links it too and takes nothing:
// no release, no stage, no release.stage). Its first tag (v1.0, a tag's create) makes a released
// release in each. Ana makes "v1.1" by hand; the published GitHub release
// v1.1 (an annotated tag) ships it, and its commits since v1.0 (ONE compare
// call for both projects) put in each project's release only that project's
// tasks the bot may read: WEB-2 by its key, WEB-3 by the branch a merge
// commit merged, never WEB-1 (restricted) and never Billing's BIL-1 in
// Website's. Its first delivery's compare answers 502, so it ships nothing
// and its redelivery puts the tasks in. The tag's create for v1.1, and a
// replayed delivery, change nothing. A deploy and a workflow run on the
// tag's commit report stages (a newer report moves the stage, an older one
// changes nothing; release.stage told); a run on an untagged commit
// reports none. Ben (member) reads the stages; notes: no drafter -> 404,
// a drafter -> drafted from what Ana may read, Ben refused (403).
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestATagShipsEachLinkedProjectsReleaseWithItsTasksAndStages$' -v

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	releaseBusiness "github.com/akashc777/OneCamp/business/Release"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	githubController "github.com/akashc777/OneCamp/controllers/GitHub"
	releaseController "github.com/akashc777/OneCamp/controllers/Release"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// gitHubStub answers the GitHub API calls a tag makes, and counts compares:
// the next failCompares of them answer 502.
type gitHubStub struct {
	real         http.RoundTripper
	mu           sync.Mutex
	compares     []string
	failCompares int
}

func (g *gitHubStub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return g.real.RoundTrip(r)
	}
	body := `{}`
	switch p := r.URL.Path; {
	case p == "/repos/acme/shop/git/ref/tags/v1.0":
		body = `{"object": {"sha": "sha-one", "type": "commit"}}`
	case p == "/repos/acme/shop/git/ref/tags/v1.1":
		body = `{"object": {"sha": "tag-object", "type": "tag"}}`
	case p == "/repos/acme/shop/git/tags/tag-object":
		body = `{"object": {"sha": "sha-two", "type": "commit"}}`
	case strings.HasPrefix(p, "/repos/acme/shop/compare/"):
		g.mu.Lock()
		g.compares = append(g.compares, strings.TrimPrefix(p, "/repos/acme/shop/compare/"))
		failing := g.failCompares > 0
		if failing {
			g.failCompares--
		}
		g.mu.Unlock()
		if failing {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"message": "Server Error"}`)), Request: r}, nil
		}
		body = `{"total_commits": 5, "commits": [
			{"sha": "c1", "commit": {"message": "WEB-2 check the copy"}},
			{"sha": "c2", "commit": {"message": "Merge pull request #9 from acme/web-3-newsletter\n\nNewsletter"}},
			{"sha": "c3", "commit": {"message": "Fixes WEB-1"}},
			{"sha": "c4", "commit": {"message": "BIL-1 invoices"}},
			{"sha": "c5", "commit": {"message": "chore: bump"}}]}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

type eventTape struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (e *eventTape) Emit(_ context.Context, ev resourceEventBusiness.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func TestATagShipsEachLinkedProjectsReleaseWithItsTasksAndStages(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	stub := &gitHubStub{real: http.DefaultTransport}
	http.DefaultTransport = stub
	tape := &eventTape{}
	bus := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = tape
	// The AI edition's drafter (business/AI's init, linked in by the
	// package's other tests) is taken away, and put back after.
	drafter := releaseBusiness.RegisterNotesDrafter(nil)
	t.Cleanup(func() {
		http.DefaultTransport = stub.real
		resourceEventBusiness.Bus = bus
		releaseBusiness.RegisterNotesDrafter(drafter)
	})

	// WEB-1 (w.task) is restricted to the project's admins and its maker: the
	// bot isn't one. Billing holds BIL-1. Both link acme/shop.
	ana := w.account("ana")
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	billing, bil := uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Billing', $2, $3)`, billing, w.team, w.people["ana"])
	w.exec(`INSERT INTO tasks (id, project_id, created_by, task_number) VALUES ($1, $2, $3, 1)`, bil, billing, w.people["ana"])
	anaUID := ana.UserDgraphInfo.Uid
	w.dg.Mutate(t, []map[string]any{{"uid": "_:billing", "dgraph.type": "Project", "project_uuid": billing, "project_name": "Billing",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": anaUID}}, "project_admins": []map[string]any{{"uid": anaUID}},
		"project_tasks": []map[string]any{{"uid": "_:bil", "dgraph.type": "Task", "task_uuid": bil, "task_name": "Invoices", "task_status": "todo",
			"task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": "_:billing"}, "task_created_by": map[string]any{"uid": anaUID}}}}})
	// Archive links acme/shop too, but is archived: a tag ships nothing there.
	archive := uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Archive', $2, $3)`, archive, w.team, w.people["ana"])
	w.dg.Mutate(t, []map[string]any{{"uid": "_:archive", "dgraph.type": "Project", "project_uuid": archive, "project_name": "Archive",
		"project_deleted_at": "2026-11-01T00:00:00Z", "project_members": []map[string]any{{"uid": anaUID}},
		"project_admins": []map[string]any{{"uid": anaUID}}}})
	for i, id := range []string{w.task, w.assigned, w.mine} {
		w.exec(`UPDATE tasks SET task_number = $1 WHERE id = $2`, i+1, id)
	}
	w.exec(`INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'WEB', 4), ($2, 'BIL', 2)`, w.project, billing)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'shop', true, true, 'shop-secret', $3), ($2, 'acme', 'shop', true, true, 'shop-secret', $3),
		($4, 'acme', 'shop', true, true, 'shop-secret', $3)`, w.project, billing, w.people["ana"], archive)
	w.exec(`INSERT INTO integrations (entity_type, entity_id, provider, access_token) VALUES ('org', $1, 'github', 'test-token')`, uuid.Nil)
	w.exec(`INSERT INTO github_task_activities (id, task_id, activity_type, title, created_at) VALUES ($1, $2, 'pr_opened', 'Copy checked by legal', NOW())`,
		uuid.New(), w.assigned)
	// Older pull requests of WEB-2: a brief names the newest three a task.
	for i, title := range []string{"Copy draft", "Copy outline", "Copy brief"} {
		w.exec(`INSERT INTO github_task_activities (id, task_id, activity_type, title, created_at) VALUES ($1, $2, 'pr_merged', $3, $4)`,
			uuid.New(), w.assigned, title, time.Now().Add(-time.Duration(i+1)*time.Hour))
	}

	// handle sends a delivery and answers how it ended: completed or failed.
	handle := func(event, delivery string, payload map[string]any) (string, string) {
		t.Helper()
		payload["repository"] = map[string]any{"full_name": "acme/shop", "name": "shop", "owner": map[string]any{"login": "acme"}, "default_branch": "main"}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/integration/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", delivery)
		mac := hmac.New(sha256.New, []byte("shop-secret"))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		githubController.HandleGitHubWebhook(rec, req)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			var status, failure string
			_ = w.env.PG.QueryRow(`SELECT status, COALESCE(error_message, '') FROM github_webhook_deliveries WHERE delivery_id = $1`,
				delivery).Scan(&status, &failure)
			if status == "completed" || status == "failed" {
				return status, failure
			}
		}
		t.Fatalf("delivery %s (%s) wasn't handled", delivery, event)
		return "", ""
	}
	deliver := func(event, delivery string, payload map[string]any) {
		t.Helper()
		if status, failure := handle(event, delivery, payload); status != "completed" {
			t.Fatalf("delivery %s (%s) failed: %s", delivery, event, failure)
		}
	}
	type row struct {
		id, state, tag, sha string
		tasks               []string
	}
	release := func(project, name string) row {
		t.Helper()
		var r row
		if err := w.env.PG.QueryRow(`SELECT id, state, COALESCE(tag, ''), COALESCE(tag_sha, '') FROM releases
			WHERE project_uuid = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`, project, name).Scan(&r.id, &r.state, &r.tag, &r.sha); err != nil {
			t.Fatalf("%s's release %s: %v", project, name, err)
		}
		// One git add puts its tasks in with one statement (added_at ties):
		// they are a set, read in task id order.
		rows, err := w.env.PG.Query(`SELECT task_uuid::text || ':' || source FROM release_tasks WHERE release_id = $1 ORDER BY task_uuid`, r.id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			r.tasks = append(r.tasks, s)
		}
		return r
	}

	// The first tag: a released release in each project, nothing to compare.
	deliver("create", "d-tag-1", map[string]any{"ref": "v1.0", "ref_type": "tag"})
	for _, p := range []string{w.project, billing} {
		if r := release(p, "v1.0"); r.state != "released" || r.tag != "v1.0" || r.sha != "sha-one" || len(r.tasks) != 0 {
			t.Errorf("v1.0 in %s: %+v", p, r)
		}
	}

	// Ana's planned v1.1 ships by the published GitHub release.
	call := func(who string, h http.HandlerFunc, params map[string]string, method string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		var body io.Reader = http.NoBody
		if method == http.MethodPost && params["release_id"] == "" {
			body = strings.NewReader(`{"name": "v1.1"}`)
		}
		h(rec, httptest.NewRequest(method, "/", body).WithContext(c))
		return rec.Code, rec.Body.String()
	}
	if code, body := call("ana", releaseController.CreateRelease, map[string]string{"project_uuid": w.project}, http.MethodPost); code != http.StatusOK {
		t.Fatalf("ana makes v1.1: %d %s", code, body)
	}
	byHand := release(w.project, "v1.1").id
	// GitHub's compare answers 502 the first time: the delivery fails and
	// ships nothing, so GitHub's redelivery of it puts the tasks in.
	stub.mu.Lock()
	stub.failCompares = 1
	stub.mu.Unlock()
	if status, _ := handle("release", "d-rel-11", map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.1", "draft": false}}); status != "failed" {
		t.Errorf("the delivery whose compare answered 502 %s, want failed", status)
	}
	deliver("release", "d-rel-11", map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.1", "draft": false}})
	web := release(w.project, "v1.1")
	want := []string{w.assigned + ":git", w.mine + ":git"}
	sort.Strings(want)
	if web.id != byHand || web.state != "released" || web.tag != "v1.1" || web.sha != "sha-two" || strings.Join(web.tasks, ",") != strings.Join(want, ",") {
		t.Errorf("Website's v1.1 %+v, want ana's (%s) shipped with %v", web, byHand, want)
	}
	if b := release(billing, "v1.1"); b.state != "released" || strings.Join(b.tasks, ",") != bil+":git" {
		t.Errorf("Billing's v1.1 %+v, want BIL-1 alone", b)
	}
	// The tag's own create and a replayed delivery change nothing, and the
	// commits were compared once a delivery for both projects (the 502 and
	// the redelivery's), never for the create or the replay.
	deliver("create", "d-tag-11", map[string]any{"ref": "v1.1", "ref_type": "tag"})
	deliver("release", "d-rel-11", map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.1"}})
	if strings.Join(stub.compares, ",") != "v1.0...v1.1,v1.0...v1.1" {
		t.Errorf("compares %v, want v1.0...v1.1 twice", stub.compares)
	}

	// Stages: a deploy and runs on the tag's commit; newer reports win. The
	// deploy reports in GitHub's order, in progress (09:59) then its success
	// (10:00), which moves the kept stage; the run's success (10:05) then an
	// older in_progress (10:01) changes nothing.
	deliver("deployment_status", "d-dep-start", map[string]any{"deployment_status": map[string]any{"state": "in_progress", "environment": "production",
		"log_url": "https://github.com/acme/shop/actions/runs/1", "updated_at": "2026-11-06T09:59:00Z"},
		"deployment": map[string]any{"sha": "sha-two", "ref": "v1.1"}})
	deliver("deployment_status", "d-dep", map[string]any{"deployment_status": map[string]any{"state": "success", "environment": "production",
		"log_url": "https://github.com/acme/shop/actions/runs/1", "updated_at": "2026-11-06T10:00:00Z"},
		"deployment": map[string]any{"sha": "sha-two", "ref": "v1.1"}})
	deliver("workflow_run", "d-run-done", map[string]any{"action": "completed", "workflow_run": map[string]any{"name": "CI", "status": "completed",
		"conclusion": "success", "head_sha": "sha-two", "head_branch": "v1.1", "updated_at": "2026-11-06T10:05:00Z"}})
	deliver("workflow_run", "d-run-late", map[string]any{"action": "in_progress", "workflow_run": map[string]any{"name": "CI", "status": "in_progress",
		"head_sha": "sha-two", "head_branch": "v1.1", "updated_at": "2026-11-06T10:01:00Z"}})
	deliver("workflow_run", "d-run-untagged", map[string]any{"action": "completed", "workflow_run": map[string]any{"name": "Nightly", "status": "completed",
		"conclusion": "success", "head_sha": "sha-zzz", "head_branch": "main", "updated_at": "2026-11-06T10:05:00Z"}})
	var stages []string
	rows, err := w.env.PG.Query(`SELECT kind || ':' || name || ':' || status FROM release_stages WHERE release_id = $1 ORDER BY kind, name`, web.id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		stages = append(stages, s)
	}
	rows.Close()
	if strings.Join(stages, ",") != "build:CI:success,deploy:production:success" {
		t.Errorf("Website's v1.1 stages %v", stages)
	}
	var elsewhere int
	_ = w.env.PG.QueryRow(`SELECT COUNT(*) FROM release_stages WHERE name = 'Nightly'`).Scan(&elsewhere)
	var told []string
	tape.mu.Lock()
	for _, e := range tape.events {
		if e.Kind == resourceEventBusiness.ReleaseStage && e.ResourceUUID == web.id {
			told = append(told, e.Data["kind"]+":"+e.Data["name"]+":"+e.Data["status"])
		}
	}
	tape.mu.Unlock()
	if elsewhere != 0 || strings.Join(told, ",") != "deploy:production:in_progress,deploy:production:success,build:CI:success" {
		t.Errorf("an untagged run kept %d stages; release.stage told Website's v1.1 %v, want the deploy's two reports and the run's success", elsewhere, told)
	}

	t.Run("an archived project linked to the repository ships nothing, keeps no stage and is told nothing", func(t *testing.T) {
		var releases, stages int
		_ = w.env.PG.QueryRow(`SELECT COUNT(*) FROM releases WHERE project_uuid = $1`, archive).Scan(&releases)
		_ = w.env.PG.QueryRow(`SELECT COUNT(*) FROM release_stages s JOIN releases r ON r.id = s.release_id WHERE r.project_uuid = $1`,
			archive).Scan(&stages)
		bill := release(billing, "v1.1")
		stranger := 0
		tape.mu.Lock()
		for _, e := range tape.events {
			if e.Kind == resourceEventBusiness.ReleaseStage && e.ResourceUUID != web.id && e.ResourceUUID != bill.id {
				stranger++
			}
		}
		tape.mu.Unlock()
		if releases != 0 || stages != 0 || stranger != 0 || bill.state != "released" {
			t.Errorf("Archive: %d releases, %d stages, %d release.stage told elsewhere; Billing's v1.1 %s: want none, and Billing's released",
				releases, stages, stranger, bill.state)
		}
	})

	// Ben, a member, reads the stages and the tasks he may see.
	at := map[string]string{"project_uuid": w.project, "release_id": web.id}
	if code, body := call("ben", releaseController.GetRelease, at, http.MethodGet); code != http.StatusOK ||
		!strings.Contains(body, `"production"`) || !strings.Contains(body, w.mine) {
		t.Errorf("ben reads v1.1: %d %s", code, body)
	}

	// Notes: none without a drafter; drafted from what the asker may read.
	if code, _ := call("ana", releaseController.DraftReleaseNotes, at, http.MethodPost); code != http.StatusNotFound {
		t.Errorf("no drafter: %d, want 404", code)
	}
	var brief releaseBusiness.NotesBrief
	releaseBusiness.RegisterNotesDrafter(func(_ context.Context, _ string, b releaseBusiness.NotesBrief) (string, error) {
		brief = b
		return "### New\n- The newsletter", nil
	})
	code, body := call("ana", releaseController.DraftReleaseNotes, at, http.MethodPost)
	if code != http.StatusOK || !strings.Contains(body, "The newsletter") || len(brief.Tasks) != 2 ||
		!strings.Contains(fmt.Sprint(brief.Tasks), "[Copy checked by legal Copy draft Copy outline]") {
		t.Errorf("ana drafts: %d %s brief %+v", code, body, brief)
	}
	if code, _ := call("ben", releaseController.DraftReleaseNotes, at, http.MethodPost); code != http.StatusForbidden {
		t.Errorf("ben drafts: %d, want 403", code)
	}
}
