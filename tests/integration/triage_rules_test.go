//go:build integration

package integration_test

// linear-21 H2: rules act on work as it arrives in triage, and a rota says
// who is on triage duty.
// Run: PKGS=./tests/integration/ with each test named exactly.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	triageController "github.com/akashc777/OneCamp/controllers/Triage"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// arrivals is a world with triage on, the engine running, and ana's API
// token: made(name) sends a task in by the API door (it waits in triage).
type arrivals struct {
	w       *workWorld
	ana     userModels.UserInfo
	project uuid.UUID
	token   string
	routes  http.Handler
}

func newArrivals(t *testing.T) *arrivals {
	t.Helper()
	t.Setenv("JWT_SECRET", "triage-rules-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	workflowBusiness.Start(ctx)
	a := &arrivals{w: w, ana: w.account("ana"), project: uuid.MustParse(w.project), routes: router.Routes()}
	made, err := apiTokenBusiness.CreateToken(ctx, "triage-rules", apiTokenBusiness.AllScopes, 30, a.ana.UserPostgresInfo.Id, nil, true)
	if err != nil {
		t.Fatalf("make a token: %v", err)
	}
	a.token = made.Plaintext
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project}
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, scope, json.RawMessage(`true`),
		policyBusiness.Changer{UserID: a.ana.UserPostgresInfo.Id}); err != nil {
		t.Fatal(err)
	}
	return a
}

// call sends a request through the router with ana's token.
func (a *arrivals) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.routes.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (a *arrivals) made(t *testing.T, name string) string {
	t.Helper()
	code, out := a.call(t, http.MethodPost, "/v1/tasks", map[string]any{"task_name": name, "project_uuid": a.w.project})
	data, _ := out["data"].(map[string]any)
	if code != http.StatusOK || data["in_triage"] != "true" {
		t.Fatalf("send %q in: %d %v", name, code, out)
	}
	return data["task_uuid"].(string)
}

func (a *arrivals) rule(t *testing.T, owner *userModels.UserInfo, in workflowBusiness.WorkflowInput) uuid.UUID {
	t.Helper()
	in.IsActive, in.TriggerType = true, workflowModel.TriggerTaskArrived
	r, err := workflowBusiness.CreateProjectRule(context.Background(), owner, a.project, in)
	if err != nil {
		t.Fatalf("an admin makes an arrival rule: %v", err)
	}
	return r.Id
}

// waiting is the tasks waiting in the project's triage, as ana sees them.
func (a *arrivals) waiting(t *testing.T) map[string]bool {
	t.Helper()
	q, err := triageBusiness.Triage.Waiting(context.Background(), &a.ana, a.project)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range q {
		out[r.TaskUUID.String()] = true
	}
	return out
}

// decision is the decision kept for a task: what, why and by whom.
func (a *arrivals) decision(t *testing.T, task string) (string, string, string) {
	t.Helper()
	var d, reason, by string
	err := a.w.env.PG.QueryRow(`SELECT decision, COALESCE(reason, ''), decided_by::text FROM triage_decisions WHERE task_uuid = $1`, task).Scan(&d, &reason, &by)
	if err != nil {
		t.Fatalf("the decision on %s: %v", task, err)
	}
	return d, reason, by
}

// A rule on arriving work decides it through the queue as its owner: work
// sent in by the API is accepted into a status; work it names as spam is
// declined with the reason the requester is told. Each run is kept.
func TestARuleAcceptsOrDeclinesWorkThatArrivesByTheAPI(t *testing.T) {
	a := newArrivals(t)
	id := a.rule(t, &a.ana, workflowBusiness.WorkflowInput{
		Name: "Sort what arrives by the API",
		Conditions: workflowBusiness.Conditions{Items: []workflowBusiness.Condition{
			{Field: "source", Op: "is", Value: "api"}, {Field: "name", Op: "not_contains", Value: "casino"}}},
		Actions:     []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionTriage, Decision: "accept", Status: "inProgress"}},
		ElseActions: []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionTriage, Decision: "decline", Text: "That isn't work for this team"}},
	})
	good := a.made(t, "Fix the footer")
	runsOf(t, a.project, id, 1)
	spam := a.made(t, "Win at the casino")
	runs := runsOf(t, a.project, id, 2)
	for _, r := range runs {
		if r.Status != workflowModel.RunOK {
			t.Fatalf("run %+v", r)
		}
	}
	if w := a.waiting(t); w[good] || w[spam] {
		t.Fatalf("still waiting: %v", w)
	}
	ana := a.ana.UserPostgresInfo.Id.String()
	if d, _, by := a.decision(t, good); d != "accept" || by != ana {
		t.Errorf("the good one: %s by %s", d, by)
	}
	if d, reason, by := a.decision(t, spam); d != "decline" || reason != "That isn't work for this team" || by != ana {
		t.Errorf("the spam: %s (%s) by %s", d, reason, by)
	}
}

// The rota says who is on duty, and a rule gives arriving work to them.
func TestARuleGivesArrivingWorkToWhoeverIsOnDuty(t *testing.T) {
	a := newArrivals(t)
	ben := a.w.people["ben"].String()
	start := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	params := map[string]string{"project_uuid": a.w.project}
	body := map[string]any{"people": []string{ben}, "weeks": 1, "start": start}
	if code, out := a.w.jfCall("ana", triageController.SetRota, http.MethodPut, "/", params, body); code != http.StatusOK || !strings.Contains(out, `"on_duty":"`+ben+`"`) {
		t.Fatalf("ana sets the rota: %d %s", code, out)
	}
	id := a.rule(t, &a.ana, workflowBusiness.WorkflowInput{
		Name:    "Whoever is on duty takes it",
		Actions: []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionAssign, Assign: workflowBusiness.AssignOnDuty}},
	})
	task := a.made(t, "Translate the pricing page")
	if r := runsOf(t, a.project, id, 1)[0]; r.Status != workflowModel.RunOK {
		t.Fatalf("run %+v", r)
	}
	var got struct {
		Q []struct {
			Assignee struct {
				UUID string `json:"user_uuid"`
			} `json:"task_assignee"`
		} `json:"q"`
	}
	q := fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_assignee { user_uuid } } }`, task)
	if err := json.Unmarshal(a.w.dg.Query(t, q), &got); err != nil || len(got.Q) != 1 || got.Q[0].Assignee.UUID != ben {
		t.Fatalf("assigned to %+v, want ben: %v", got, err)
	}
	if !a.waiting(t)[task] {
		t.Error("giving it to someone decided it: it still waits to be accepted")
	}
}

// The rota is the project admins' to set, in the app: ben (a member) is
// refused, ana (an admin) sets it, and everyone in the project reads it.
func TestAMembersRotaPutIsRefused(t *testing.T) {
	a := newArrivals(t)
	params := map[string]string{"project_uuid": a.w.project}
	body := map[string]any{"people": []string{a.w.people["ben"].String()}, "weeks": 1, "start": "2026-11-02"}
	if code, out := a.w.jfCall("ben", triageController.SetRota, http.MethodPut, "/", params, body); code == http.StatusOK {
		t.Fatalf("ben set the rota: %s", out)
	}
	if code, out := a.w.jfCall("ana", triageController.SetRota, http.MethodPut, "/", params, body); code != http.StatusOK || !strings.Contains(out, `"on_duty"`) {
		t.Fatalf("ana sets the rota: %d %s", code, out)
	}
	if code, out := a.w.jfCall("ben", triageController.Rota, http.MethodGet, "/", params, nil); code != http.StatusOK || !strings.Contains(out, a.w.people["ben"].String()) || !strings.Contains(out, `"can_manage":false`) {
		t.Fatalf("ben reads the rota: %d %s", code, out)
	}
	if code, out := a.w.jfCall("ana", triageController.Rota, http.MethodGet, "/", params, nil); code != http.StatusOK || !strings.Contains(out, `"can_manage":true`) || !strings.Contains(out, `"next_change":""`) {
		t.Fatalf("ana reads the rota she may set (one person: no change): %d %s", code, out)
	}
	if code, _ := a.w.jfCall("eve", triageController.Rota, http.MethodGet, "/", params, nil); code == http.StatusOK {
		t.Fatal("eve, outside the project, read the rota")
	}
	if code, out := a.w.jfCall("ana", triageController.SetRota, http.MethodPut, "/", params, map[string]any{"people": []string{uuid.NewString()}, "weeks": 1, "start": "2026-11-02"}); code != http.StatusBadRequest {
		t.Fatalf("a rota naming an outsider: %d %s", code, out)
	}
}

// Tokens act as members: through ana's own token (she is an admin) neither
// an arrival rule nor the rota is set.
func TestATokenSetsNoRuleAndNoRota(t *testing.T) {
	a := newArrivals(t)
	rule := map[string]any{"name": "Through a token", "is_active": true, "trigger_type": workflowModel.TriggerTaskArrived,
		"actions": []map[string]any{{"type": workflowBusiness.ActionTriage, "decision": "accept"}}}
	if code, out := a.call(t, http.MethodPost, "/project/"+a.w.project+"/rules", rule); code == http.StatusOK || code == http.StatusCreated {
		t.Fatalf("a token made a rule: %d %v", code, out)
	}
	rota := map[string]any{"people": []string{a.w.people["ben"].String()}, "weeks": 1, "start": "2026-11-02"}
	if code, out := a.call(t, http.MethodPut, "/project/"+a.w.project+"/triage/rota", rota); code == http.StatusOK {
		t.Fatalf("a token set the rota: %d %v", code, out)
	}
	rules, err := workflowBusiness.ListProjectRules(context.Background(), a.project)
	if err != nil || len(rules) != 0 {
		t.Fatalf("rules %d, %v", len(rules), err)
	}
}

// A rule whose owner has left the project acts on nothing: its run is kept
// as skipped, and the work waits in triage, undecided.
func TestARuleWhoseOwnerLeftLeavesTheTaskWaiting(t *testing.T) {
	a := newArrivals(t)
	fay := a.w.account("fay")
	id := a.rule(t, &fay, workflowBusiness.WorkflowInput{
		Name:    "Accept everything",
		Actions: []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionTriage, Decision: "accept"}},
	})
	a.w.leaveProject(t, "fay")
	task := a.made(t, "Fix the footer")
	if r := runsOf(t, a.project, id, 1)[0]; r.Status != workflowModel.RunSkipped {
		t.Fatalf("run %+v, want skipped", r)
	}
	if !a.waiting(t)[task] {
		t.Fatal("the task left triage")
	}
}

// leaveProject takes name out of the project's members and admins in the
// graph, as leaving it does.
func (w *workWorld) leaveProject(t *testing.T, name string) {
	t.Helper()
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
		U []struct {
			UID string `json:"uid"`
		} `json:"u"`
	}
	q := fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { uid } u(func: eq(user_uuid, %q)) { uid } }`, w.project, w.people[name].String())
	if err := json.Unmarshal(w.dg.Query(t, q), &got); err != nil || len(got.P) != 1 || len(got.U) != 1 {
		t.Fatalf("the project and %s in the graph: %v %+v", name, err, got)
	}
	who := map[string]any{"uid": got.U[0].UID}
	del, _ := json.Marshal(map[string]any{"uid": got.P[0].UID, "project_members": who, "project_admins": who})
	if _, err := dgraphInit.DoCommitNow(context.Background(), &api.Request{Mutations: []*api.Mutation{{DeleteJson: del}}}); err != nil {
		t.Fatalf("%s leaves the project: %v", name, err)
	}
}
