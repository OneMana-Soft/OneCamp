//go:build integration
// +build integration

package integration_test

// jira-01 J-F: "formula fields from the Formula Engine (#61) ... all free",
// through the handlers, against real Postgres 12 and Dgraph (seedWorkWorld:
// ana is Website's admin, ben its member, fay an admin who isn't a member,
// cy in the team only). One subtest per rule of PLAN-JF.txt.
//
// Run: PKGS=./tests/integration/ ... integ_rel.sh <tree> <log> 'TestJF'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// jfCall runs a handler as who, with the route's params, and answers its
// status and body.
func (w *workWorld) jfCall(who string, h http.HandlerFunc, method, target string, params map[string]string, body any) (int, string) {
	w.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, _ := w.signedIn(who)
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, target, rd).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
	// JSON answers compacted ("refetch":true), so the checks read one way;
	// a CSV as it is.
	var compact bytes.Buffer
	if json.Compact(&compact, rec.Body.Bytes()) == nil {
		return rec.Code, compact.String()
	}
	return rec.Code, rec.Body.String()
}

// setRole is by giving who the role to in Website, through the handler behind
// POST /project/{project_uuid}/members/{user_uuid}/role; it answers its status
// and body.
func (w *workWorld) setRole(by, who, to string) (int, string) {
	w.t.Helper()
	return w.jfCall(by, projectController.SetMemberRole, http.MethodPost, "/",
		map[string]string{"project_uuid": w.project, "user_uuid": w.people[who].String()}, map[string]string{"role": to})
}

// setEditWho is by choosing Website's "Who can change tasks" (a nil value
// gives it back to the workspace's), through the handler behind
// PUT /project/{project_uuid}/policies/{key}; it answers its status and body.
func (w *workWorld) setEditWho(by string, value any) (int, string) {
	w.t.Helper()
	return w.jfCall(by, projectController.SetProjectPolicy, http.MethodPut, "/",
		map[string]string{"project_uuid": w.project, "key": policyBusiness.ProjectsEditWho.Key}, map[string]any{"value": value})
}

type jfField struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Formula string `json:"formula"`
	Result  string `json:"result"`
	Error   string `json:"error"`
	Hidden  bool   `json:"hidden"`
}

func TestJFFormulaTaskFields(t *testing.T) {
	w := seedWorkWorld(t)
	project := map[string]string{"project_uuid": w.project}

	// create makes a field as who, by what the app sends.
	create := func(who string, in map[string]any) (int, jfField, string) {
		t.Helper()
		code, body := w.jfCall(who, taskFieldController.CreateField, http.MethodPost, "/", project, in)
		var out struct {
			Data jfField `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return code, out.Data, body
	}
	must := func(in map[string]any) jfField {
		t.Helper()
		code, f, body := create("ana", in)
		if code != http.StatusOK || f.ID == "" {
			t.Fatalf("ana makes %v: %d %s", in["name"], code, body)
		}
		return f
	}
	fields := func(who string) (map[string]jfField, string) {
		t.Helper()
		code, body := w.jfCall(who, taskFieldController.ListFields, http.MethodGet, "/", project, nil)
		if code != http.StatusOK {
			t.Fatalf("%s lists the fields: %d %s", who, code, body)
		}
		var out struct {
			Data struct {
				Fields []jfField `json:"fields"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		byName := map[string]jfField{}
		for _, f := range out.Data.Fields {
			byName[f.Name] = f
		}
		return byName, body
	}
	// info is a task's values as its panel reads them for who.
	info := func(who, task string) (map[string]any, string) {
		t.Helper()
		code, body := w.jfCall(who, taskController.GetTaskInfo, http.MethodGet, "/", map[string]string{"task_uuid": task}, nil)
		if code != http.StatusOK {
			t.Fatalf("%s opens %s: %d %.300s", who, task, code, body)
		}
		var out struct {
			Data struct {
				Fields map[string]any `json:"task_fields"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return out.Data.Fields, body
	}
	set := func(who, task, field string, v any) (int, string) {
		return w.jfCall(who, taskFieldController.SetTaskField, http.MethodPost, "/", nil,
			map[string]any{"task_uuid": task, "field_id": field, "value": v})
	}

	hours := must(map[string]any{"name": "Hours", "type": "number"})
	notes := must(map[string]any{"name": "Notes", "type": "text"})
	budget := must(map[string]any{"name": "Budget", "type": "money", "currency": "USD"})
	double := must(map[string]any{"name": "Double", "type": "formula", "formula": "{Hours} * 2"})
	spend := must(map[string]any{"name": "Spend", "type": "formula", "formula": "{Budget} * 2"})
	spendMore := must(map[string]any{"name": "Spend more", "type": "formula", "formula": "{Spend} + {Hours}"})
	says := must(map[string]any{"name": "Says", "type": "formula", "formula": `{Priority} & " " & {Hours}`})
	for f, v := range map[string]any{hours.ID: 3, budget.ID: 125000} {
		if code, body := set("ana", w.task, f, v); code != http.StatusOK {
			t.Fatalf("ana sets %s: %d %s", f, code, body)
		}
	}
	// The launch post is high (a built-in the formulas read).
	var got struct {
		T []struct {
			UID string `json:"uid"`
		} `json:"t"`
	}
	raw := w.dg.Query(t, fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { uid } }`, w.task))
	if err := json.Unmarshal(raw, &got); err != nil || len(got.T) != 1 {
		t.Fatalf("the launch post's node: %s %v", raw, err)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": got.T[0].UID, "task_priority": "high"}})
	// The report reads its projects from each reader's own list.
	w.onTheirLists("ana", "ben")

	t.Run("project admins define formula fields; members can't; all free", func(t *testing.T) {
		if code, _, body := create("ben", map[string]any{"name": "Mine", "type": "formula", "formula": "1"}); code != http.StatusForbidden {
			t.Errorf("ben makes a formula field: %d %s", code, body)
		}
		// Made above on the free plan, with no plan gate; the list says what each gives.
		byName, body := fields("ana")
		if f := byName["Double"]; f.Type != "formula" || f.Formula != "{Hours} * 2" || f.Result != "number" {
			t.Errorf("Double as listed: %+v (%s)", f, body)
		}
		if !strings.Contains(body, `"builtins"`) || !strings.Contains(body, `"Due date"`) {
			t.Errorf("the built-ins a formula reads aren't listed: %.400s", body)
		}
	})

	t.Run("a formula that can't be read is refused in words", func(t *testing.T) {
		for src, want := range map[string]string{
			"{Nothing} + 1": "Nothing",
			"{Hours} +":     "",
			"":              "formula",
		} {
			code, _, body := create("ana", map[string]any{"name": "Bad", "type": "formula", "formula": src})
			if code != http.StatusBadRequest || !strings.Contains(body, want) {
				t.Errorf("Bad = %q: %d %s", src, code, body)
			}
		}
		// Double reading Spend more, which reads... Hours only: fine. Double
		// reading Quad, which reads Double, loops.
		quad := must(map[string]any{"name": "Quad", "type": "formula", "formula": "{Double} * 2"})
		code, body := w.jfCall("ana", taskFieldController.UpdateField, http.MethodPost, "/",
			map[string]string{"project_uuid": w.project, "field_id": double.ID}, map[string]any{"name": "Double", "formula": "{Quad} * 2"})
		if code != http.StatusBadRequest || !strings.Contains(body, "its own value") {
			t.Errorf("Double reading Quad: %d %s", code, body)
		}
		_ = quad
	})

	t.Run("built-ins and custom fields are worked out at read", func(t *testing.T) {
		values, body := info("ana", w.task)
		if values[double.ID] != 6.0 || values[says.ID] != "High 3" || values[spend.ID] != 2500.0 || values[spendMore.ID] != 2503.0 {
			t.Errorf("ana's panel: %v (%.400s)", values, body)
		}
	})

	t.Run("money and what reads it are for project admins only", func(t *testing.T) {
		for _, who := range []string{"ana", "fay"} {
			if values, _ := info(who, w.task); values[spend.ID] != 2500.0 {
				t.Errorf("%s, an admin, reads Spend: %v", who, values[spend.ID])
			}
		}
		values, body := info("ben", w.task)
		if _, ok := values[spend.ID]; ok {
			t.Errorf("ben, a member, reads Spend: %.400s", body)
		}
		// Q1: the money field itself, as the formula over it.
		if _, ok := values[budget.ID]; ok {
			t.Errorf("ben reads Budget: %.400s", body)
		}
		byName, body := fields("ben")
		if !byName["Spend"].Hidden || !byName["Budget"].Hidden || byName["Double"].Hidden {
			t.Errorf("ben's fields say what's hidden: %.500s", body)
		}
		if values[double.ID] != 6.0 {
			t.Errorf("ben reads Double, which reads no money: %v", values[double.ID])
		}
	})

	t.Run("a hidden input hides the result, through other formulas", func(t *testing.T) {
		values, body := info("ben", w.task)
		if _, ok := values[spendMore.ID]; ok {
			t.Errorf("Spend more reads Spend, which reads Budget; ben reads it: %.400s", body)
		}
		if byName, _ := fields("ben"); !byName["Spend more"].Hidden {
			t.Error("Spend more isn't hidden from ben")
		}
	})

	t.Run("a formula's value is never written", func(t *testing.T) {
		code, body := set("ana", w.task, double.ID, 5)
		if code != http.StatusBadRequest || !strings.Contains(body, "worked out") {
			t.Errorf("ana sets Double: %d %s", code, body)
		}
		if values, _ := info("ana", w.task); values[double.ID] != 6.0 {
			t.Errorf("Double after the refused set: %v", values[double.ID])
		}
	})

	t.Run("project admins preview a formula as they write it", func(t *testing.T) {
		// The route, as the app's router serves it.
		var preview http.Handler
		_ = chi.Walk(router.Routes().(chi.Routes), func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
			if method == http.MethodPost && route == "/project/{project_uuid}/fields/preview" {
				preview = h
			}
			return nil
		})
		if preview == nil {
			t.Fatal("no POST /project/{project_uuid}/fields/preview")
		}
		call := func(who string, in map[string]any) (int, string) {
			return w.jfCall(who, preview.ServeHTTP, http.MethodPost, "/", project, in)
		}
		if code, body := call("ben", map[string]any{"formula": "{Hours} * 3"}); code != http.StatusForbidden || !strings.Contains(body, "admins") {
			t.Errorf("ben previews: %d %s", code, body)
		}
		code, body := call("ana", map[string]any{"formula": "{Hours} * 3"})
		if code != http.StatusOK || !strings.Contains(body, `"result":"number"`) || !strings.Contains(body, `"values":[]`) || strings.Contains(body, `"error"`) {
			t.Errorf("ana previews {Hours} * 3: %d %s", code, body)
		}
		code, body = call("ana", map[string]any{"formula": "{Double} + 1", "field_id": double.ID})
		if code != http.StatusOK || !strings.Contains(body, "its own value") {
			t.Errorf("ana previews Double reading itself: %d %s", code, body)
		}
		code, body = call("ana", map[string]any{"formula": "{Nothing} + 1"})
		if code != http.StatusOK || !strings.Contains(body, "Nothing") {
			t.Errorf("ana previews {Nothing}: %d %s", code, body)
		}
	})

	t.Run("a set of a field a formula reads tells the app to read the task again", func(t *testing.T) {
		code, body := set("ana", w.task, hours.ID, 3)
		if code != http.StatusOK || !strings.Contains(body, `"refetch":true`) {
			t.Errorf("ana sets Hours, which Double reads: %d %s", code, body)
		}
		code, body = set("ana", w.task, notes.ID, "Draft")
		if code != http.StatusOK || strings.Contains(body, `"refetch":true`) {
			t.Errorf("ana sets Notes, which no formula reads: %d %s", code, body)
		}
	})

	t.Run("every reader carries the result", func(t *testing.T) {
		list := map[string]string{"project_uuid": w.project}
		readers := []struct {
			name   string
			h      http.HandlerFunc
			target string
		}{
			{"list", projectController.GetProjectTaskList, "/?pageSize=50&pageIndex=0"},
			{"kanban", projectController.GetProjectTaskListForKanban, "/"},
			{"board column", projectController.GetProjectBoardColumn, "/?status=todo"},
		}
		for _, r := range readers {
			code, body := w.jfCall("ana", r.h, http.MethodGet, r.target, list, nil)
			if code != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`%q:2500`, spend.ID)) || !strings.Contains(body, fmt.Sprintf(`%q:6`, double.ID)) {
				t.Errorf("ana's %s: %d %.300s", r.name, code, body)
			}
			code, body = w.jfCall("ben", r.h, http.MethodGet, r.target, list, nil)
			if code != http.StatusOK || strings.Contains(body, spend.ID) || !strings.Contains(body, fmt.Sprintf(`%q:6`, double.ID)) {
				t.Errorf("ben's %s: %d %.300s", r.name, code, body)
			}
		}
		// The export: a column for each field the reader may read.
		csvOf := func(who string) string {
			code, body := w.jfCall(who, projectController.GetProjectTaskList, http.MethodGet, "/?format=csv&tz=UTC", list, nil)
			if code != http.StatusOK {
				t.Fatalf("%s exports: %d %.200s", who, code, body)
			}
			return body
		}
		if ana := csvOf("ana"); !strings.Contains(ana, "Spend") || !strings.Contains(ana, ",2500,") || !strings.Contains(ana, ",6,") {
			t.Errorf("ana's export: %.600s", ana)
		}
		if ben := csvOf("ben"); strings.Contains(ben, "Spend") || strings.Contains(ben, "Budget") || !strings.Contains(ben, ",6,") {
			t.Errorf("ben's export: %.600s", ben)
		}
		// The report slices by a formula as by its result; never by a hidden one.
		// report is the fields who's report slices by, by their names.
		report := func(who string) map[string]string {
			code, body := w.jfCall(who, projectController.Report, http.MethodGet, "/project/report?projects="+url.QueryEscape(w.project), nil, nil)
			if code != http.StatusOK {
				t.Fatalf("%s's report: %d %.200s", who, code, body)
			}
			var r struct {
				Data struct {
					Insights struct {
						Fields []struct {
							By   string `json:"by"`
							Name string `json:"name"`
						} `json:"fields"`
					} `json:"insights"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(body), &r)
			by := map[string]string{}
			for _, f := range r.Data.Insights.Fields {
				by[f.Name] = f.By
			}
			return by
		}
		if by := report("ana"); by["Double"] != "field:"+double.ID || by["Spend"] != "field:"+spend.ID {
			t.Errorf("ana's report doesn't slice by the formulas: %v", by)
		}
		if by := report("ben"); by["Spend"] != "" || by["Budget"] != "" || by["Double"] != "field:"+double.ID {
			t.Errorf("ben's report: %v", by)
		}
	})

	t.Run("a list can't be narrowed by a formula yet", func(t *testing.T) {
		b, _ := json.Marshal([]map[string]any{{"id": "field_" + strings.ReplaceAll(double.ID, "-", ""), "value": []string{"any"}}})
		code, body := w.jfCall("ana", projectController.GetProjectTaskList, http.MethodGet, "/?pageSize=50&pageIndex=0&filters="+url.QueryEscape(string(b)),
			map[string]string{"project_uuid": w.project}, nil)
		if code != http.StatusBadRequest || !strings.Contains(strings.ToLower(body), "formula") {
			t.Errorf("a filter on Double: %d %.300s", code, body)
		}
	})

	t.Run("deletes are soft: a formula over a deleted field says so until its restore", func(t *testing.T) {
		code, body := w.jfCall("ana", taskFieldController.DeleteField, http.MethodPost, "/",
			map[string]string{"project_uuid": w.project, "field_id": hours.ID}, nil)
		if code != http.StatusOK {
			t.Fatalf("ana deletes Hours: %d %s", code, body)
		}
		values, body := info("ana", w.task)
		if e, _ := values[double.ID].(map[string]any); e == nil || !strings.Contains(fmt.Sprint(e["error"]), "deleted") {
			t.Errorf("Double over a deleted Hours: %.400s", body)
		}
		if byName, _ := fields("ana"); !strings.Contains(byName["Double"].Formula, "{Deleted field}") {
			t.Errorf("Double's formula as listed: %q", byName["Double"].Formula)
		}
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM task_field_values WHERE field_id = $1`, hours.ID).Scan(&n); err != nil || n != 1 {
			t.Errorf("Hours' value is kept for its restore: %d %v", n, err)
		}
	})
}

// Migration 583 goes down and up again cleanly with formula fields in it: the
// type check's 'formula' comes and goes; down keeps every formula (no data is
// lost) but puts the live formula fields in the trash, as the code before J-F
// reads live fields only and has no formula type; up again leaves them there,
// formula intact, for a project admin to bring back. A field trashed before
// keeps its own deletion, and no other field is touched.
func TestTaskFieldFormulaMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "583_task_field_formula."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	const project, double, old, hours = "00000000-0000-4000-8000-000000000583", "00000000-0000-4000-8000-0000000005f1",
		"00000000-0000-4000-8000-0000000005f2", "00000000-0000-4000-8000-0000000005f3"
	if _, err := env.PG.Exec(`INSERT INTO task_fields (id, project_id, name, type, formula, deleted_at) VALUES
		($2, $1, 'Double', 'formula', '{#`+hours+`} * 2', NULL),
		($3, $1, 'Old', 'formula', '1 + 1', '2026-11-01T00:00:00Z'),
		($4, $1, 'Hours', 'number', NULL, NULL)`, project, double, old, hours); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// field is what a field is after each step: its type, formula and whether it's live.
	field := func(i int, id string) string {
		t.Helper()
		var typ, formula string
		var live, column bool
		if err := env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'task_fields' AND column_name = 'formula')`).Scan(&column); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		text := `''`
		if column {
			text = `COALESCE(formula, '')`
		}
		if err := env.PG.QueryRow(`SELECT type, `+text+`, deleted_at IS NULL FROM task_fields WHERE id = $1`, id).Scan(&typ, &formula, &live); err != nil {
			t.Fatalf("step %d: read %s: %v", i, id, err)
		}
		return fmt.Sprintf("%s %q live=%v", typ, formula, live)
	}
	for i, step := range []struct {
		sql      string
		formula  bool
		doubleIs string
	}{
		{"", true, `formula "{#` + hours + `} * 2" live=true`},
		{down, false, `formula "{#` + hours + `} * 2" live=false`},
		{up, true, `formula "{#` + hours + `} * 2" live=false`},
		{down, false, `formula "{#` + hours + `} * 2" live=false`},
		{up, true, `formula "{#` + hours + `} * 2" live=false`},
	} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		var check string
		var only bool
		var lengths int
		if err := env.PG.QueryRow(`SELECT (SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'task_fields_type_check'),
			EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'task_fields_formula_only_check'),
			(SELECT count(*) FROM pg_constraint WHERE conname = 'task_fields_formula_length_check')`).Scan(&check, &only, &lengths); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		// The formula column stays through a down, and its length check with it.
		if lengths != 1 {
			t.Fatalf("step %d: %d formula length checks, want 1", i, lengths)
		}
		if strings.Contains(check, "'formula'") != step.formula || only != step.formula {
			t.Fatalf("step %d: type check %s, formula-only check %v", i, check, only)
		}
		if got := field(i, double); got != step.doubleIs {
			t.Errorf("step %d: Double is %s, want %s", i, got, step.doubleIs)
		}
		if got := field(i, old); got != `formula "1 + 1" live=false` {
			t.Errorf("step %d: Old, trashed before, is %s", i, got)
		}
		if got := field(i, hours); got != `number "" live=true` {
			t.Errorf("step %d: Hours is %s", i, got)
		}
	}
	// Old keeps its own deletion time; only what was live went to the trash.
	var kept bool
	if err := env.PG.QueryRow(`SELECT deleted_at = '2026-11-01T00:00:00Z' FROM task_fields WHERE id = $1`, old).Scan(&kept); err != nil || !kept {
		t.Errorf("Old's deletion time moved: %v %v", kept, err)
	}
}
