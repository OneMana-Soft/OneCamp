//go:build integration
// +build integration

package integration_test

// Every route of a project refuses who it isn't for, as Work Access decides
// (business/WorkAccess through controllers/ProjectAccess). In seedWorkWorld's
// Website project:
//   - eve, outside its team and the project, and cy, in the team but not the
//     project, find nothing at any of its routes (404). The statuses' routes
//     and the older reads (info, members, files, lists, columns) tell them
//     they may not (403) instead, naming nothing of the project;
//   - ben, its member, is refused (403) everything that changes its settings,
//     money, rules, statuses, fields, forms, key, types or people;
//   - the project is as it was after all of that, and ana, its admin, and ben
//     still read it.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestEveryProjectRouteRefusesWhoItIsntFor$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	approvalsController "github.com/akashc777/OneCamp/controllers/Approvals"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	goalController "github.com/akashc777/OneCamp/controllers/Goal"
	invoiceController "github.com/akashc777/OneCamp/controllers/Invoice"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	projectTemplateController "github.com/akashc777/OneCamp/controllers/ProjectTemplate"
	projectUpdateController "github.com/akashc777/OneCamp/controllers/ProjectUpdate"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	taskKeysController "github.com/akashc777/OneCamp/controllers/TaskKeys"
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	workflowController "github.com/akashc777/OneCamp/controllers/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEveryProjectRouteRefusesWhoItIsntFor(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	// Each route's other ids name nothing: the project is checked first.
	other := uuid.NewString()
	call := func(who string, h http.HandlerFunc, body any) (int, string) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("project_uuid", w.project)
		for _, k := range []string{"field_id", "form_id", "invoice_id", "rule_id", "run_id", "type_id", "status_id", "update_id"} {
			rc.URLParams.Add(k, other)
		}
		rc.URLParams.Add("key", "tasks.edit")
		rc.URLParams.Add("user_uuid", w.people["ana"].String())
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", rd).WithContext(c))
		out := rec.Body.String()
		if len(out) > 160 {
			out = out[:160]
		}
		return rec.Code, out
	}
	type route struct {
		what string
		h    http.HandlerFunc
		body any
	}
	// What anyone in the project may do: read it, and the changes its members
	// make by default (posting an update, undoing a rule's run).
	viewed := []route{
		{"reads its backlog", cycleController.Backlog, nil},
		{"reads its critical path", taskController.CriticalPath, nil},
		{"reads its tags", taskController.ProjectTags, nil},
		{"reads its fields", taskFieldController.ListFields, nil},
		{"reads its forms", formController.ListForms, nil},
		{"reads its glance", projectController.ProjectGlance, nil},
		{"reads who can change its tasks", projectController.ProjectPolicies, nil},
		{"reads its timeline", projectController.ProjectTimeline, nil},
		{"reads its goals", goalController.ProjectGoals, nil},
		{"reads its key", taskKeysController.ProjectKey, nil},
		{"reads its types", taskKeysController.ProjectTypes, nil},
		{"reads its time", timeEntryController.ProjectTime, nil},
		{"reads its rules", workflowController.ListProjectRules, nil},
		{"reads its rules' runs", workflowController.ListProjectRuleRuns, nil},
		{"undoes a rule's run", workflowController.UndoProjectRuleRun, nil},
		{"reads its status rules", approvalsController.ListStatusRules, nil},
		{"reads its updates", projectUpdateController.ListUpdates, nil},
		{"drafts an update", projectUpdateController.DraftUpdate, nil},
		{"posts an update", projectUpdateController.PostUpdate, map[string]any{"health": "on_track", "body": "<p>All good</p>"}},
		{"edits an update", projectUpdateController.EditUpdate, map[string]any{"health": "at_risk", "body": "<p>Not good</p>"}},
		{"deletes an update", projectUpdateController.DeleteUpdate, nil},
	}
	// What only the project's admins do.
	managed := []route{
		{"adds a field", taskFieldController.CreateField, map[string]any{"name": "Cost", "kind": "number"}},
		{"orders its fields", taskFieldController.ReorderFields, map[string]any{"ids": []string{other}}},
		{"changes a field", taskFieldController.UpdateField, map[string]any{"name": "Price"}},
		{"deletes a field", taskFieldController.DeleteField, nil},
		{"saves a form", formController.SaveForm, map[string]any{"name": "Requests"}},
		{"deletes a form", formController.DeleteForm, nil},
		{"reads a form's submissions", formController.FormSubmissions, nil},
		{"lists its invoices", invoiceController.ListInvoices, nil},
		{"makes an invoice", invoiceController.CreateInvoice, map[string]any{"from": "2026-10-01", "until": "2026-10-31"}},
		{"reads an invoice", invoiceController.GetInvoice, nil},
		{"changes an invoice", invoiceController.UpdateInvoice, map[string]any{"number": "INV-9"}},
		{"marks an invoice paid", invoiceController.SetInvoiceStatus, map[string]any{"status": "paid"}},
		{"deletes an invoice", invoiceController.DeleteInvoice, nil},
		{"reads its rates", timeEntryController.ProjectRates, nil},
		{"sets its rates", timeEntryController.SetProjectRates, map[string]any{"currency": "USD", "default_rate_cents": 9500}},
		{"clears its rates", timeEntryController.ClearProjectRates, nil},
		{"changes its key", taskKeysController.SetProjectKey, map[string]any{"key": "BEN"}},
		{"adds a type", taskKeysController.CreateProjectType, map[string]any{"name": "Spike"}},
		{"changes a type", taskKeysController.UpdateProjectType, map[string]any{"name": "Chore"}},
		{"archives a type", taskKeysController.ArchiveProjectType, nil},
		{"chooses who can change its tasks", projectController.SetProjectPolicy, map[string]any{"value": "admins"}},
		{"adds a rule", workflowController.CreateProjectRule, map[string]any{"name": "Close it"}},
		{"changes a rule", workflowController.UpdateProjectRule, map[string]any{"name": "Close it now"}},
		{"turns a rule off", workflowController.SetProjectRuleActive, map[string]any{"active": false}},
		{"deletes a rule", workflowController.DeleteProjectRule, nil},
		{"saves it as a template", projectTemplateController.SaveProjectAsTemplate, map[string]any{"name": "Ben's copy"}},
		{"adds a status rule", approvalsController.SaveStatusRule, map[string]any{"status": "done"}},
		{"deletes a status rule", approvalsController.DeleteStatusRule, map[string]any{"status": "done"}},
		{"renames it", projectController.UpdateProjectName, map[string]any{"project_uuid": w.project, "project_name": "Ben's site"}},
		{"restores it", projectController.UnArchiveProject, map[string]any{"project_uuid": w.project}},
		{"makes ana a viewer", projectController.SetMemberRole, map[string]any{"role": "viewer"}},
	}
	for _, who := range []string{"eve", "cy"} {
		for _, r := range append(append([]route{}, viewed...), managed...) {
			if code, body := call(who, r.h, r.body); code != http.StatusNotFound {
				t.Errorf("%s, outside the project, %s: %d %s, want 404", who, r.what, code, body)
			}
		}
	}
	for _, r := range managed {
		if code, body := call("ben", r.h, r.body); code != http.StatusForbidden {
			t.Errorf("ben, its member, %s: %d %s, want 403", r.what, code, body)
		}
	}

	// The statuses' own check: members read them, admins change them.
	statusReads := []route{{"reads its statuses", taskStatusController.List, nil}}
	statusChanges := []route{
		{"adds a status", taskStatusController.Create, map[string]any{"name": "Blocked", "category": "started"}},
		{"changes a status", taskStatusController.Update, map[string]any{"name": "Stuck"}},
		{"deletes a status", taskStatusController.Delete, nil},
		{"reorders its statuses", taskStatusController.Reorder, map[string]any{"ids": []string{other}}},
		{"orders its statuses", taskStatusController.Order, map[string]any{"order": []string{"todo"}}},
	}
	for _, who := range []string{"eve", "cy"} {
		for _, r := range append(append([]route{}, statusReads...), statusChanges...) {
			if code, body := call(who, r.h, r.body); code != http.StatusForbidden {
				t.Errorf("%s, outside the project, %s: %d %s, want 403", who, r.what, code, body)
			}
		}
	}
	for _, r := range statusChanges {
		if code, body := call("ben", r.h, r.body); code != http.StatusForbidden {
			t.Errorf("ben, its member, %s: %d %s, want 403", r.what, code, body)
		}
	}

	// A board column is asked for by its status: without one the route
	// answers 400 before it looks at the project.
	askedFor := func(query string, h http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			r.URL.RawQuery = query
			h(rw, r)
		}
	}
	// The older reads say "Not Authorised" to someone not in the project.
	older := []route{
		{"reads its info", projectController.ProjectInfo, nil},
		{"lists its members", projectController.GetDgraphProjectMemberList, nil},
		{"lists its members and admins", projectController.GetDgraphProjectCombinedMemberInfo, nil},
		{"lists its files", projectController.GetDgraphProjectAttachmentList, nil},
		{"lists its tasks", projectController.GetProjectTaskList, nil},
		{"lists its board's tasks", projectController.GetProjectTaskListForKanban, nil},
		{"reads its board's columns", askedFor("status=todo", projectController.GetProjectBoardColumn), nil},
	}
	for _, who := range []string{"eve", "cy"} {
		for _, r := range older {
			if code, body := call(who, r.h, r.body); code != http.StatusForbidden {
				t.Errorf("%s, outside the project, %s: %d %s, want 403", who, r.what, code, body)
			}
		}
	}

	// Its people: ben may not change them, and cy can't add himself.
	person := func(who string) string { return w.people[who].String() }
	people := []struct {
		what string
		h    http.HandlerFunc
		who  string
	}{
		{"adds cy", projectController.AddMemberToProject, "cy"},
		{"takes ana off", projectController.RemoveMemberFromProject, "ana"},
		{"makes himself an admin", projectController.AddAdminMemberToProject, "ben"},
		{"takes fay's admin away", projectController.RemoveAdminRoleFromMember, "fay"},
	}
	for _, p := range people {
		body := map[string]string{"project_uuid": w.project, "user_uuid": person(p.who)}
		if code, got := call("ben", p.h, body); code != http.StatusForbidden {
			t.Errorf("ben, its member, %s: %d %s, want 403", p.what, code, got)
		}
	}
	if code, got := call("cy", projectController.AddMemberToProject, map[string]string{"project_uuid": w.project, "user_uuid": person("cy")}); code != http.StatusNotFound {
		t.Errorf("cy, outside the project, adds himself: %d %s, want 404", code, got)
	}

	// The project is as it was.
	place := func(who string) (member, admin bool, name string) {
		t.Helper()
		p, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, w.project, w.account(who).UserDgraphInfo.Uid)
		if err != nil {
			t.Fatal(err)
		}
		return p.IsProjectMember > 0, p.IsProjectAdmin > 0, p.Name
	}
	if member, admin, name := place("ben"); !member || admin || name != "Website" {
		t.Errorf("ben after all that: member %v, admin %v, project named %q; want a member, not an admin, of Website", member, admin, name)
	}
	if member, _, _ := place("cy"); member {
		t.Error("cy is in the project after all that")
	}
	if member, _, _ := place("ana"); !member {
		t.Error("ana was taken off the project")
	}
	if _, admin, _ := place("fay"); !admin {
		t.Error("fay lost her admin")
	}
	for _, c := range []struct {
		who, what string
		h         http.HandlerFunc
	}{
		{"ben", "reads its glance", projectController.ProjectGlance},
		{"ben", "reads its statuses", taskStatusController.List},
		{"ana", "reads its rates", timeEntryController.ProjectRates},
		{"ana", "reads its statuses", taskStatusController.List},
	} {
		if code, body := call(c.who, c.h, nil); code != http.StatusOK {
			t.Errorf("%s, in the project, %s: %d %s, want 200", c.who, c.what, code, body)
		}
	}
}
