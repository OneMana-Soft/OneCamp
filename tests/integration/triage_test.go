//go:build integration
// +build integration

package integration_test

// The Triage Queue end to end (package 9 R5a), against real Postgres 12 and
// Dgraph: a project turns triage on through its policy; a task that comes in
// by the API waits there, out of the project's counts but readable alone,
// and the queue shows it with who asked; accepting it into a status brings it
// back, and a second decision finds it decided; declining cancels it; turning
// triage off lets what waits back into the counts, undecided.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTriage -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	triageController "github.com/akashc777/OneCamp/controllers/Triage"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTriageHoldsIncomingWorkUntilItIsDecided(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben, eve := w.account("ana"), w.account("ben"), w.account("eve")
	project := uuid.MustParse(w.project)
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}
	asAna := taskread.ActingFor(ctx, ana.UserPostgresInfo.Id.String())
	open := func() int {
		t.Helper()
		g, err := projectDomain.GetProjectGlance(asAna, w.project, time.Now())
		if err != nil {
			t.Fatalf("the project's counts: %v", err)
		}
		return g.Open
	}
	turn := func(on bool) {
		t.Helper()
		v, _ := json.Marshal(on)
		if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, scope, v, policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
			t.Fatalf("turning triage %v: %v", on, err)
		}
	}
	first, second, third := uuid.MustParse(w.task), uuid.MustParse(w.assigned), uuid.MustParse(w.mine)
	before := open()

	// Off, as every project starts: nothing waits.
	if held, err := triageBusiness.Hold(ctx, project, first, triageBusiness.FromAPI, triageBusiness.Requester{}); held || err != nil {
		t.Fatalf("triage off held it: %v %v", held, err)
	}

	turn(true)
	benID := ben.UserPostgresInfo.Id
	if held, err := triageBusiness.Hold(ctx, project, first, triageBusiness.FromAPI, triageBusiness.Requester{UserUUID: &benID}); !held || err != nil {
		t.Fatalf("triage on didn't hold it: %v %v", held, err)
	}
	if got := open(); got != before-1 {
		t.Fatalf("the counts kept what waits: %d open, want %d", got, before-1)
	}
	if task, _, err := taskBusiness.TaskFor(asAna, first, &ana, workAccess.NeedView); err != nil || task == nil {
		t.Fatalf("what waits is hidden from no one, but its own read failed: %v", err)
	}
	queue, err := triageBusiness.Triage.Waiting(ctx, &ana, project)
	if err != nil || len(queue) != 1 || queue[0].TaskUUID != first || queue[0].Name != "Write the launch post" ||
		queue[0].Source != "api" || queue[0].RequesterUUID == nil || *queue[0].RequesterUUID != benID {
		t.Fatalf("the queue: %+v, %v", queue, err)
	}

	// Someone outside the project decides nothing.
	if err := triageBusiness.Triage.Decide(ctx, &eve, first, triageBusiness.Choice{Decision: triageBusiness.Accept}); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Fatalf("eve decided: %v", err)
	}

	// Ben accepts it into In progress: back in the counts, decided once.
	if err := triageBusiness.Triage.Decide(ctx, &ben, first, triageBusiness.Choice{Decision: triageBusiness.Accept, Status: "inProgress", Reason: "This week"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := open(); got != before {
		t.Fatalf("an accepted task stayed out of the counts: %d open, want %d", got, before)
	}
	if got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid); got == nil || got.Status != "inProgress" {
		t.Fatalf("accepted into %+v", got)
	}
	if err := triageBusiness.Triage.Decide(ctx, &ana, first, triageBusiness.Choice{Decision: triageBusiness.Decline}); !errors.Is(err, triageBusiness.ErrNotWaiting) {
		t.Fatalf("a second decision: %v", err)
	}
	var kept int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM triage_decisions WHERE task_uuid = $1 AND decision = 'accept' AND reason = 'This week' AND decided_by = $2`, first, benID).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the decision kept: %d, %v", kept, err)
	}

	// Ana declines another: it is canceled.
	if _, err := triageBusiness.Hold(ctx, project, second, triageBusiness.FromForm, triageBusiness.Requester{Email: "sam@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := triageBusiness.Triage.Decide(ctx, &ana, second, triageBusiness.Choice{Decision: triageBusiness.Decline, Reason: "Not something we make"}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.assigned, ana.UserDgraphInfo.Uid); got == nil || got.Status != "canceled" {
		t.Fatalf("declined into %+v", got)
	}

	// Two more come in (the seed's project has three tasks).
	more := func(name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, w.project, w.people["ana"])
		var out struct {
			Q []struct {
				UID string `json:"uid"`
			} `json:"q"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(project_uuid, %q)) { uid } }`, w.project)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("the project's node: %+v %v", out, err)
		}
		w.dg.Mutate(t, []map[string]any{{"uid": out.Q[0].UID, "project_tasks": []map[string]any{{"uid": "_:t", "dgraph.type": "Task",
			"task_uuid": id.String(), "task_name": name, "task_status": "todo", "task_deleted_at": "0001-01-01T00:00:00Z",
			"task_project": map[string]any{"uid": out.Q[0].UID}, "task_created_by": map[string]any{"uid": ana.UserDgraphInfo.Uid}}}}})
		return id
	}

	// Ana snoozes one for a day: it still waits, out of the counts and in the
	// queue with when it comes back; the snooze is kept; and it can still be
	// accepted after.
	snoozed := more("Answer the press mail")
	if _, err := triageBusiness.Hold(ctx, project, snoozed, triageBusiness.FromForm, triageBusiness.Requester{Email: "press@example.com"}); err != nil {
		t.Fatal(err)
	}
	beforeSnooze := open()
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	if err := triageBusiness.Triage.Decide(ctx, &ana, snoozed, triageBusiness.Choice{Decision: triageBusiness.Snooze, Until: until, Reason: "After the launch"}); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if got := open(); got != beforeSnooze {
		t.Fatalf("a snoozed task came back into the counts: %d open, want %d", got, beforeSnooze)
	}
	queue, err = triageBusiness.Triage.Waiting(ctx, &ana, project)
	var back *time.Time
	for _, r := range queue {
		if r.TaskUUID == snoozed {
			back = r.SnoozedUntil
		}
	}
	if err != nil || back == nil || !back.Equal(until) {
		t.Fatalf("the snoozed task in the queue, back at %v: %+v, %v", back, queue, err)
	}
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM triage_decisions WHERE task_uuid = $1 AND decision = 'snooze' AND snoozed_until = $2 AND reason = 'After the launch'`, snoozed, until).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the snooze kept: %d, %v", kept, err)
	}
	if err := triageBusiness.Triage.Decide(ctx, &ana, snoozed, triageBusiness.Choice{Decision: triageBusiness.Accept}); err != nil {
		t.Fatalf("accepting a snoozed task: %v", err)
	}
	if got := open(); got != beforeSnooze+1 {
		t.Fatalf("an accepted snoozed task stayed out of the counts: %d open, want %d", got, beforeSnooze+1)
	}

	// Ana marks another a duplicate of the launch post, through the Task
	// Writer: it is canceled, linked as duplicating the launch post, and the
	// launch post says it was asked for again.
	dup := more("Write a launch blog post")
	if _, err := triageBusiness.Hold(ctx, project, dup, triageBusiness.FromChat, triageBusiness.Requester{UserUUID: &benID}); err != nil {
		t.Fatal(err)
	}
	if err := triageBusiness.Triage.Decide(ctx, &ana, dup, triageBusiness.Choice{Decision: triageBusiness.Duplicate, DuplicateOf: first, Reason: "Same post"}); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, dup.String(), ana.UserDgraphInfo.Uid); got == nil || got.Status != "canceled" {
		t.Fatalf("a duplicate went into %+v", got)
	}
	links, err := taskBusiness.Links(asAna, dup, &ana)
	if err != nil || len(links) != 1 || links[0].Kind != "duplicates" || links[0].Direction != "out" || links[0].Task.UUID != w.task {
		t.Fatalf("the duplicate's links: %+v, %v", links, err)
	}
	if said := string(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_comments { comment_text } } }`, w.task))); !strings.Contains(said, "was marked a duplicate of this.") {
		t.Fatalf("the launch post wasn't told it was asked for again: %s", said)
	}
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM triage_decisions WHERE task_uuid = $1 AND decision = 'duplicate' AND duplicate_of = $2`, dup, first).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the duplicate kept: %d, %v", kept, err)
	}

	// A third waits; turning triage off lets it back into the counts,
	// undecided, and empties the queue.
	if _, err := triageBusiness.Hold(ctx, project, third, triageBusiness.FromAPI, triageBusiness.Requester{}); err != nil {
		t.Fatal(err)
	}
	waiting := open()
	turn(false)
	if got := open(); got != waiting+1 {
		t.Fatalf("triage off kept what waits out of the counts: %d open, want %d", got, waiting+1)
	}
	if queue, err := triageBusiness.Triage.Waiting(ctx, &ana, project); queue != nil || err != nil {
		t.Fatalf("triage off, the queue: %+v, %v", queue, err)
	}
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM triage_items WHERE task_uuid = $1 AND decided_at IS NULL`, third).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("turning triage off decided what waited: %d, %v", kept, err)
	}
}

// A task made through the public API in a project with triage on waits in
// it, and the answer says so; with triage off the same call says nothing of
// triage (package 9 R5a's API door, controllers/V1 CreateTaskV1).
func TestTriageTakesTheAPIsTasks(t *testing.T) {
	t.Setenv("JWT_SECRET", "triage-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	made, err := apiTokenBusiness.CreateToken(ctx, "triage", apiTokenBusiness.AllScopes, 30, ana.UserPostgresInfo.Id, nil, true)
	if err != nil {
		t.Fatalf("make a token: %v", err)
	}
	routes := router.Routes()
	create := func(name string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"task_name": name, "project_uuid": w.project})
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+made.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		data, _ := out["data"].(map[string]any)
		return rec.Code, data
	}
	if code, data := create("Fix the footer"); code != http.StatusOK || data["in_triage"] != "false" {
		t.Fatalf("triage off: %d %v", code, data)
	}
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, scope, json.RawMessage(`true`), policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	code, data := create("Translate the pricing page")
	if code != http.StatusOK || data["in_triage"] != "true" {
		t.Fatalf("triage on: %d %v", code, data)
	}
	queue, err := triageBusiness.Triage.Waiting(ctx, &ana, project)
	if err != nil || len(queue) != 1 || queue[0].TaskUUID.String() != data["task_uuid"] || queue[0].Source != "api" ||
		queue[0].RequesterUUID == nil || *queue[0].RequesterUUID != ana.UserPostgresInfo.Id {
		t.Fatalf("the queue: %+v, %v", queue, err)
	}
}

// The doors that make work from outside the app (linear-21 H1): an issue
// opened on a linked repository waits in triage with who asked, and an issue
// whose link sets a status for new issues is the team's work at once.
// (without-ai's form: the AI edition also checks create_task as a token or MCP
// calls it, the assistant in a member's own session, and tasks made from a
// chat, all through its AI tools.)
func TestTriageTakesTheWorkThatArrivesFromOutsideTheApp(t *testing.T) {
	refuseGitHub(t)
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, scope, json.RawMessage(`true`), policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	// Two repositories linked to the project: one sets no status for a new
	// issue, one does. Each is this test's own (links are cached by
	// repository).
	repoOf := func(rules string) string {
		t.Helper()
		name := "triage-" + uuid.NewString()[:8]
		if _, err := w.env.PG.Exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, auto_create_tasks, created_by, automation_rules)
			VALUES ($1, 'acme', $2, true, true, true, $3, $4::jsonb)`, w.project, name, ana.UserPostgresInfo.Id, rules); err != nil {
			t.Fatal(err)
		}
		return name
	}
	open := func(repo, title string) string {
		t.Helper()
		url := "https://github.com/acme/" + repo + "/issues/1"
		raw, _ := json.Marshal(map[string]any{"action": "opened", "sender": map[string]any{"login": "octo"},
			"issue":      map[string]any{"number": 1, "title": title, "body": "", "state": "open", "html_url": url},
			"repository": map[string]any{"full_name": "acme/" + repo, "name": repo, "owner": map[string]any{"login": "acme"}}})
		if err := githubBusiness.HandleGitHubWebhookEvent(ctx, "issues", raw); err != nil {
			t.Fatalf("GitHub's issue opened: %v", err)
		}
		var id string
		if err := w.env.PG.QueryRow(`SELECT id FROM tasks WHERE github_issue_url = $1`, url).Scan(&id); err != nil {
			t.Fatalf("the issue's task: %v", err)
		}
		return id
	}
	issue := open(repoOf(`{"pr_merged": "done"}`), "Login page answers 500")
	ruled := open(repoOf(`{"issue_opened": "inProgress"}`), "Dark mode")

	queue, err := triageBusiness.Triage.Waiting(ctx, &ana, project)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]triageBusiness.Request{}
	for _, r := range queue {
		byName[r.Name] = r
	}
	if len(queue) != 1 {
		t.Errorf("waiting: %+v", queue)
	}
	if r, ok := byName["Login page answers 500"]; !ok || r.TaskUUID.String() != issue || r.Source != "github" || r.RequesterName != "@octo on GitHub" || r.RequesterUUID != nil {
		t.Errorf("the issue waits as %+v (%v)", r, ok)
	}
	for _, name := range []string{"Dark mode"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%q waits in triage", name)
		}
	}
	if got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, ruled, ana.UserDgraphInfo.Uid); got == nil || got.Status != "inProgress" {
		t.Errorf("the ruled issue's task: %+v", got)
	}
}

// A task deleted while it waits in triage stops waiting (CR-288 F2): nobody
// can decide a deleted task, so it must not hold a place under the project's
// MaxWaiting, where a pile of deleted spam would stop triage taking work, nor
// sit in the lists' waiting set. The item stays (items are never deleted),
// decided with no decision kept, since nobody made one. Restoring the task
// brings it back as the team's work, not into triage: a task goes through
// triage once.
func TestATaskDeletedWhileItWaitsLeavesTriage(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project, task := uuid.MustParse(w.project), uuid.MustParse(w.task)
	on, _ := json.Marshal(true)
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}, on,
		policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if held, err := triageBusiness.Hold(ctx, project, task, triageBusiness.FromForm, triageBusiness.Requester{Email: "spam@example.com"}); !held || err != nil {
		t.Fatalf("triage on didn't hold it: %v %v", held, err)
	}
	waits := func() (int, bool) {
		t.Helper()
		n, err := triageModel.CountWaiting(ctx, project)
		set, err2 := taskread.Waiting(ctx)
		if err != nil || err2 != nil {
			t.Fatalf("what waits: %v %v", err, err2)
		}
		return n, set[w.task]
	}
	if n, listed := waits(); n != 1 || !listed {
		t.Fatalf("before the delete: %d waiting, in the lists' set %v", n, listed)
	}

	dg, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil || dg == nil {
		t.Fatalf("the task: %+v %v", dg, err)
	}
	if err := taskBusiness.ArchiveTaskByTaskUUID(ctx, task, dg, &ana.UserDgraphInfo); err != nil {
		t.Fatalf("ana deletes the task: %v", err)
	}
	// The wait ends where every sink of a deleted task's entry runs: the
	// Outbox (business/WorkFeed sinks).
	settled := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; {
			_ = outboxBusiness.Drain(ctx)
			n, listed := waits()
			if n == 0 && !listed {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d still waiting in the project's triage, in the lists' set %v", what, n, listed)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	settled("a deleted task")
	var items, decisions int
	if err := w.env.PG.QueryRow(`SELECT (SELECT count(*) FROM triage_items WHERE task_uuid = $1 AND decided_at IS NOT NULL),
		(SELECT count(*) FROM triage_decisions WHERE task_uuid = $1)`, task).Scan(&items, &decisions); err != nil || items != 1 || decisions != 0 {
		t.Fatalf("the item kept, decided: %d; decisions kept: %d; %v", items, decisions, err)
	}

	if err := taskBusiness.UnArchiveTaskByTaskUUID(ctx, task, dg, &ana.UserDgraphInfo); err != nil {
		t.Fatalf("ana restores the task: %v", err)
	}
	settled("a restored task")
	if err := triageBusiness.Triage.Decide(ctx, &ana, task, triageBusiness.Choice{Decision: triageBusiness.Accept}); !errors.Is(err, triageBusiness.ErrNotWaiting) {
		t.Fatalf("a restored task was still in triage to decide: %v", err)
	}
}

// The triage routes' answers are the web's contract (CR2 F2), called through
// their handlers as each person: GET /project/{project_uuid}/triage says
// whether triage is on (the projects.triage policy), what waits ("requests",
// [] when nothing does, never null) and whether the reader may decide
// (can_decide: NeedEditTask, so a viewer reads false and an admin true); POST
// /task/triage/{task_uuid}/decide refuses a task id or a time it can't read
// with a 400, refuses a viewer, and decides for an admin.
func TestTheTriageRoutesAnswerTheirContract(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	queue := func(who string) map[string]any {
		t.Helper()
		code, body := w.jfCall(who, triageController.Queue, http.MethodGet, "/", map[string]string{"project_uuid": w.project}, nil)
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &out); code != http.StatusOK || err != nil || out.Data == nil {
			t.Fatalf("%s reads the queue: %d %s", who, code, body)
		}
		return out.Data
	}
	requests := func(d map[string]any) []any {
		t.Helper()
		r, ok := d["requests"].([]any)
		if !ok {
			t.Fatalf("requests is %#v, want a list", d["requests"])
		}
		return r
	}
	decide := func(who, task string, body map[string]string) (int, string) {
		t.Helper()
		return w.jfCall(who, triageController.Decide, http.MethodPost, "/", map[string]string{"task_uuid": task}, body)
	}

	// Off, as every project starts: on false, nothing waiting is [].
	if d := queue("ana"); d["on"] != false || len(requests(d)) != 0 || d["can_decide"] != true {
		t.Fatalf("triage off, ana reads %v", d)
	}
	on, _ := json.Marshal(true)
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}, on,
		policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if d := queue("ana"); d["on"] != true || len(requests(d)) != 0 || d["can_decide"] != true {
		t.Fatalf("triage on with nothing waiting, ana reads %v", d)
	}

	// Ben, a viewer, reads what waits but may not decide.
	if code, body := w.setRole("ana", "ben", "viewer"); code != http.StatusOK {
		t.Fatalf("ben as a viewer: %d %s", code, body)
	}
	if _, err := triageBusiness.Hold(ctx, uuid.MustParse(w.project), uuid.MustParse(w.task), triageBusiness.FromAPI, triageBusiness.Requester{}); err != nil {
		t.Fatal(err)
	}
	d := queue("ben")
	if r := requests(d); d["on"] != true || d["can_decide"] != false || len(r) != 1 {
		t.Fatalf("ben reads %v", d)
	}
	if first, _ := requests(d)[0].(map[string]any); first["task_uuid"] != w.task {
		t.Fatalf("what waits: %v", d["requests"])
	}

	if code, body := decide("ana", "not-a-task", map[string]string{"decision": "accept"}); code != http.StatusBadRequest || !strings.Contains(body, "That isn't a task.") {
		t.Errorf("a task id that isn't one: %d %s", code, body)
	}
	if code, body := decide("ana", w.task, map[string]string{"decision": "snooze", "until": "tomorrow"}); code != http.StatusBadRequest || !strings.Contains(body, "That isn't a time to snooze it until.") {
		t.Errorf("a time that isn't one: %d %s", code, body)
	}
	if code, body := decide("ben", w.task, map[string]string{"decision": "accept"}); code == http.StatusOK {
		t.Errorf("a viewer decided: %d %s", code, body)
	}
	if code, body := decide("ana", w.task, map[string]string{"decision": "accept"}); code != http.StatusOK || !strings.Contains(body, "Decided.") {
		t.Fatalf("ana accepts: %d %s", code, body)
	}
	if d := queue("ana"); len(requests(d)) != 0 {
		t.Fatalf("after the accept, ana reads %v", d)
	}
}
