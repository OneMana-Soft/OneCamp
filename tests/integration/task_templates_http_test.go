//go:build integration
// +build integration

package integration_test

// Task templates over HTTP (linear-09), through the app's own router
// (router.Routes(), its middleware included), against real Postgres and
// Dgraph, in seedWorkWorld's Website project: each person signs in with a
// session cookie, as the app does, and a guest's link is refused at the
// sign-in (CR-167 F4).
//   - ana, its admin, keeps a template with a type, a priority, sub-tasks and
//     a required due date; ben, its member, can't keep one, and lists and
//     makes from it; cy (in the team, not the project) and eve (outside both)
//     find nothing;
//   - making asks for the required due date first (422, nothing made), then
//     makes the task through the Task Writer as ben, its sub-tasks under it;
//   - wes, a workspace admin, keeps a workspace template that the project
//     lists and makes from; ana and ben can't keep one;
//   - a template another project keeps isn't there from this one, nor a
//     project's from the workspace's routes;
//   - a deleted template is off the list and can't be used, its name is
//     free, and restoring it is refused while that name is taken;
//   - a viewer neither lists nor makes.
//
// Run: PKGS=./tests/integration/ (go test -tags=integration ./tests/integration/ -run '^TestTaskTemplatesOverHTTP$' -v)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTaskTemplatesOverHTTP(t *testing.T) {
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

	// Through the app's own router and its middleware: the session cookie is
	// the sign-in, as in the app (the CSRF check has its own tests).
	t.Setenv("CSRF_DISABLED", "1")
	t.Setenv("JWT_SECRET", "task-templates-integration-secret")
	routes := router.Routes()
	send := func(cookie, method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: cookie})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	call := func(who, method, path string, body any) (int, map[string]any) {
		t.Helper()
		session, err := helpers.SignSessionToken(w.people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		return send(session, method, path, body)
	}
	expect := func(what string, code int, out map[string]any, want int) map[string]any {
		t.Helper()
		if code != want {
			t.Fatalf("%s: %d %v, want %d", what, code, out, want)
		}
		data, _ := out["data"].(map[string]any)
		return data
	}
	names := func(who, path string) map[string]string {
		t.Helper()
		code, out := call(who, http.MethodGet, path, nil)
		data := expect(who+" lists "+path, code, out, http.StatusOK)
		got := map[string]string{}
		list, _ := data["templates"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			got[fmt.Sprint(m["name"])] = fmt.Sprint(m["id"])
		}
		return got
	}
	project := "/project/" + w.project + "/task-templates"
	shared := "/project/task-templates"
	bug := workTaxonomy.TypeBug.String()

	// A guest reaches the workspace only through a link: its token is no
	// session, so the sign-in refuses it before any template is read, as it
	// refuses no cookie at all.
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	link, err := guestBusiness.CreateResourceGrant(ctx, w.people["ana"], guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false)
	if err != nil {
		t.Fatalf("a guest link: %v", err)
	}
	for _, cookie := range []string{link.Token, ""} {
		if code, out := send(cookie, http.MethodGet, project, nil); code != http.StatusUnauthorized {
			t.Fatalf("a guest's link (%q) lists the project's templates: %d %v, want 401", cookie, code, out)
		}
	}

	// ana keeps a bug report; ben can't keep one.
	code, out := call("ana", http.MethodPost, project, map[string]any{"name": "Bug report", "description": "For bugs", "default_for": bug,
		"shape": map[string]any{"name": "Bug: ", "priority": "high", "subtasks": []map[string]any{{"name": "Reproduce"}, {"name": "Fix"}},
			"required": []string{"due"}}})
	kept := expect("ana keeps a template", code, out, http.StatusOK)
	id, _ := kept["id"].(string)
	if id == "" || kept["project_id"] != w.project || kept["default_for"] != bug {
		t.Fatalf("kept %v", kept)
	}
	// The task's name is where a made task's name starts (the maker types
	// after "Bug: "), so it is kept as typed, its space included.
	if shape, _ := kept["shape"].(map[string]any); shape["name"] != "Bug: " {
		t.Errorf("the task's name is kept as %q, want %q", shape["name"], "Bug: ")
	}
	code, out = call("ben", http.MethodPost, project, map[string]any{"name": "Mine", "shape": map[string]any{"name": "x"}})
	expect("ben keeps a template", code, out, http.StatusForbidden)

	// Who lists them, and who finds nothing.
	if got := names("ben", project); got["Bug report"] != id {
		t.Errorf("ben's list: %v", got)
	}
	code, out = call("ben", http.MethodGet, project, nil)
	if d, _ := expect("ben's list", code, out, http.StatusOK)["defaults"].(map[string]any); d[bug] != id {
		t.Errorf("a bug starts from %v, want the bug report", d)
	}

	// Each list names the types a template there may have, for the manager's
	// type and default: a project's are the built-in ones and its own, the
	// workspace's the built-in ones alone (no project's own type fits there).
	own, err := workTaxonomy.Create(ctx, uuid.MustParse(w.project), "Incident", "flag", w.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	typesOf := func(path string) map[string]string {
		t.Helper()
		code, out := call("ana", http.MethodGet, path, nil)
		got := map[string]string{}
		list, _ := expect("ana lists "+path, code, out, http.StatusOK)["types"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			got[fmt.Sprint(m["id"])] = fmt.Sprint(m["name"])
		}
		return got
	}
	if got := typesOf(project); got[bug] != "Bug" || got[own.ID.String()] != "Incident" {
		t.Errorf("the project's types: %v, want the built-in Bug and its own Incident", got)
	}
	if got := typesOf(shared); got[bug] != "Bug" || got[own.ID.String()] != "" || len(got) != 6 {
		t.Errorf("the workspace's types: %v, want the six built-in ones alone", got)
	}
	// The project's own read of its types answers them the same way.
	code, out = call("ana", http.MethodGet, "/project/"+w.project+"/types", nil)
	if code != http.StatusOK {
		t.Fatalf("ana reads the project's types: %d %v", code, out)
	}
	answered := map[string]map[string]any{}
	all, _ := out["data"].([]any)
	for _, x := range all {
		m, _ := x.(map[string]any)
		answered[fmt.Sprint(m["id"])] = m
	}
	if b, o := answered[bug], answered[own.ID.String()]; b["name"] != "Bug" || b["own"] != nil || o["own"] != true || o["icon"] != "flag" {
		t.Errorf("the project's types read %v and %v, want the built-in Bug and its own Incident (own, flag)", b, o)
	}
	for _, who := range []string{"cy", "eve"} {
		code, out = call(who, http.MethodGet, project, nil)
		expect(who+" lists", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodPost, project+"/"+id+"/make", map[string]any{"due": "2026-11-05T17:00:00Z"})
		expect(who+" makes", code, out, http.StatusNotFound)
	}

	// Making asks for the due date first, and makes nothing until it has one.
	tasks := func() int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, w.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := tasks()
	code, out = call("ben", http.MethodPost, project+"/"+id+"/make", map[string]any{"name": "Bug: the login fails", "tz": "Asia/Kolkata"})
	expect("a make without the due date", code, out, http.StatusUnprocessableEntity)
	if asks, _ := out["missing"].([]any); len(asks) != 1 || asks[0] != "due" || tasks() != before {
		t.Fatalf("a make without the due date: %v, %d tasks (was %d)", out, tasks(), before)
	}
	code, out = call("ben", http.MethodPost, project+"/"+id+"/make", map[string]any{"name": "Bug: the login fails", "due": "2026-11-05T17:00:00Z", "tz": "Asia/Kolkata"})
	made := expect("ben makes a task", code, out, http.StatusOK)
	taskID, _ := made["task_id"].(string)
	if made["subtasks"] != float64(2) || taskID == "" {
		t.Fatalf("made %v", made)
	}
	var read struct {
		T []struct {
			Name     string `json:"task_name"`
			Priority string `json:"task_priority"`
			Due      string `json:"task_due_date"`
			Subs     []struct {
				Name string `json:"task_name"`
			} `json:"task_sub_tasks"`
		} `json:"t"`
	}
	raw := w.dg.Query(t, `{ t(func: eq(task_uuid, "`+taskID+`")) { task_name task_priority task_due_date task_sub_tasks { task_name } } }`)
	if err := json.Unmarshal(raw, &read); err != nil || len(read.T) != 1 {
		t.Fatalf("reading the task: %s (%v)", raw, err)
	}
	// Its type lives in Postgres and is laid over the task when it's read
	// (business/TaskKeys); the graph never holds it.
	var typeID string
	if err := w.env.PG.QueryRow(`SELECT COALESCE(task_type_id::text, '') FROM tasks WHERE id = $1`, taskID).Scan(&typeID); err != nil {
		t.Fatalf("reading the task's type: %v", err)
	}
	if got := read.T[0]; got.Name != "Bug: the login fails" || typeID != bug || got.Priority != "high" || got.Due == "" || len(got.Subs) != 2 {
		t.Errorf("the task as made: %+v, type %q (want %s)", got, typeID, bug)
	}

	// The workspace's: wes keeps one, ana and ben can't; the project lists it and makes from it.
	for _, who := range []string{"ana", "ben"} {
		code, out = call(who, http.MethodPost, shared, map[string]any{"name": "Request", "shape": map[string]any{"name": "Request: "}})
		expect(who+" keeps a workspace template", code, out, http.StatusForbidden)
	}
	code, out = call("wes", http.MethodPost, shared, map[string]any{"name": "Request", "shape": map[string]any{"name": "Request: ", "tags": "ask"}})
	ws := expect("wes keeps a workspace template", code, out, http.StatusOK)
	wsID, _ := ws["id"].(string)
	if got := names("ben", project); got["Request"] != wsID || got["Bug report"] != id {
		t.Errorf("the project's list with the workspace's: %v", got)
	}
	if got := names("ben", shared); got["Request"] != wsID || len(got) != 1 {
		t.Errorf("the workspace's list: %v", got)
	}
	code, out = call("ben", http.MethodPost, project+"/"+wsID+"/make", map[string]any{"name": "Request: a new laptop"})
	if made := expect("ben makes from the workspace's", code, out, http.StatusOK); made["task_id"] == nil {
		t.Errorf("made %v", made)
	}

	// A template kept elsewhere isn't there from here.
	var elsewhere string
	w.exec(`INSERT INTO task_templates (project_id, name, body, created_by) VALUES ($1, 'Elsewhere', '{"name":"x"}', $2)`, uuid.NewString(), w.people["ana"])
	if err := w.env.PG.QueryRow(`SELECT id FROM task_templates WHERE name = 'Elsewhere'`).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ who, method, path string }{
		{"ben", http.MethodGet, project + "/" + elsewhere},
		{"ben", http.MethodPost, project + "/" + elsewhere + "/make"},
		{"ana", http.MethodPost, project + "/" + elsewhere + "/delete"},
		{"ana", http.MethodPost, project + "/" + wsID + "/delete"},
		{"ben", http.MethodGet, shared + "/" + id},
		{"wes", http.MethodPost, shared + "/" + id + "/delete"},
	} {
		code, out = call(c.who, c.method, c.path, map[string]any{})
		expect(c.who+" "+c.method+" "+c.path, code, out, http.StatusNotFound)
	}

	// ana changes it; ben can't.
	code, out = call("ben", http.MethodPost, project+"/"+id, map[string]any{"name": "Bug", "shape": map[string]any{"name": "Bug: "}})
	expect("ben changes it", code, out, http.StatusForbidden)
	code, out = call("ana", http.MethodPost, project+"/"+id, map[string]any{"name": "Bug", "default_for": bug, "shape": map[string]any{"name": "Bug: ", "priority": "urgent"}})
	if changed := expect("ana changes it", code, out, http.StatusOK); changed["name"] != "Bug" || changed["id"] != id {
		t.Errorf("changed %v", changed)
	}

	// Deleted: off the list, not usable, its name free; restored only once the name is free again.
	code, out = call("ana", http.MethodPost, project+"/"+id+"/delete", nil)
	expect("ana deletes it", code, out, http.StatusOK)
	if got := names("ben", project); got["Bug"] != "" {
		t.Errorf("a deleted template is listed: %v", got)
	}
	code, out = call("ben", http.MethodPost, project+"/"+id+"/make", map[string]any{"due": "2026-11-05T17:00:00Z"})
	expect("a make from a deleted template", code, out, http.StatusNotFound)
	code, out = call("ana", http.MethodPost, project, map[string]any{"name": "bug", "shape": map[string]any{"name": "Bug: "}})
	again, _ := expect("the name kept again", code, out, http.StatusOK)["id"].(string)
	code, out = call("ana", http.MethodPost, project+"/"+id+"/restore", nil)
	expect("a restore over a taken name", code, out, http.StatusBadRequest)
	code, out = call("ana", http.MethodPost, project+"/"+again+"/delete", nil)
	expect("the new one deleted", code, out, http.StatusOK)
	code, out = call("ana", http.MethodPost, project+"/"+id+"/restore", nil)
	expect("the restore", code, out, http.StatusOK)
	if got := names("ben", project); got["Bug"] != id {
		t.Errorf("the restored template: %v", got)
	}

	// A viewer neither lists nor makes.
	if err := roleModel.Set(ctx, uuid.MustParse(w.project), w.people["ben"], roleModel.Viewer, w.people["ana"]); err != nil {
		t.Fatal(err)
	}
	code, out = call("ben", http.MethodGet, project, nil)
	expect("a viewer lists", code, out, http.StatusForbidden)
	code, out = call("ben", http.MethodPost, project+"/"+id+"/make", map[string]any{"due": "2026-11-05T17:00:00Z"})
	expect("a viewer makes", code, out, http.StatusForbidden)
}
