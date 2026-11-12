//go:build integration
// +build integration

package integration_test

// L32 (linear-32) against real Postgres 12 and Dgraph, through the report's
// handler: work slices by status, labels, cycle and estimate and by every
// kind of custom field, and each slice counts its open, overdue and done
// work and its effort. Who sees what: a restricted task the reader can't
// read is in none of the slices, so its text never becomes a slice's key or
// name; a money field slices only for an admin of its project.
//
// Run: PKGS=./tests/integration/ go test -tags=integration -run TestL32 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	"github.com/akashc777/OneCamp/helpers"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
)

func TestL32InsightsByEveryFieldWithEffort(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	project := uuid.MustParse(w.project)
	ana := w.people["ana"]

	// ben and ana list Website. The launch post (ana's, restricted below to
	// the project's admins and its maker: kept from ben) is labelled
	// "secret-plan, Launch" and sized 3; the newsletter (ben's) "launch",
	// sized 5; the copy check (eve's) "copy", unsized.
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
		T []struct {
			UID  string `json:"uid"`
			UUID string `json:"task_uuid"`
		} `json:"t"`
	}
	raw := w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { uid } t(func: type(Task)) @filter(eq(task_uuid, [%q, %q, %q])) { uid task_uuid } }`,
		w.project, w.task, w.assigned, w.mine))
	if err := json.Unmarshal(raw, &got); err != nil || len(got.P) != 1 || len(got.T) != 3 {
		t.Fatalf("Website and its live tasks in the graph: %s, %v", raw, err)
	}
	made := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	labels := map[string]string{w.task: "secret-plan, Launch", w.mine: "launch", w.assigned: "copy"}
	points := map[string]any{w.task: 3, w.mine: 5}
	mut := []map[string]any{
		{"uid": w.account("ben").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
		{"uid": w.account("ana").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
	}
	for _, n := range got.T {
		m := map[string]any{"uid": n.UID, "task_created_at": made, "task_label": labels[n.UUID]}
		if p, ok := points[n.UUID]; ok {
			m["task_estimate_points"] = p
		}
		mut = append(mut, m)
	}
	w.dg.Mutate(t, mut)
	if _, err := estimateScale.Set(ctx, project, estimateScale.Scale{Kind: "fibonacci"}, ana); err != nil {
		t.Fatalf("scale: %v", err)
	}
	sprint, err := cycleModel.Create(project, "Sprint 1", time.Now().Add(-24*time.Hour), time.Now().Add(13*24*time.Hour), ana)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	for _, task := range []string{w.task, w.mine} {
		if err := cycleModel.SetTask(uuid.MustParse(task), &sprint.Id); err != nil {
			t.Fatalf("into the cycle: %v", err)
		}
	}
	field := func(in taskFieldBusiness.Input) *taskFieldModel.Field {
		t.Helper()
		f, err := taskFieldBusiness.Create(ctx, project, in, ana, "")
		if err != nil {
			t.Fatalf("field %s: %v", in.Name, err)
		}
		return f
	}
	customer := field(taskFieldBusiness.Input{Name: "Customer", Type: taskFieldBusiness.TypeText})
	budget := field(taskFieldBusiness.Input{Name: "Budget", Type: taskFieldBusiness.TypeMoney, Currency: "USD"})
	set := func(task string, f *taskFieldModel.Field, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := taskFieldModel.SetValue(ctx, uuid.MustParse(task), f.ID, b, ana); err != nil {
			t.Fatalf("%s on %s: %v", f.Name, task, err)
		}
	}
	set(w.task, customer, "Secret Corp")
	set(w.mine, customer, "Acme")
	set(w.assigned, customer, "Acme ")
	set(w.mine, budget, 500000)
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, w.account("ana").UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(ctx, task, ana, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}

	type effort struct {
		Points float64 `json:"points"`
		Hours  float64 `json:"hours"`
	}
	type slice struct {
		By     string `json:"by"`
		Key    string `json:"key"`
		Name   string `json:"name"`
		Open   int    `json:"open"`
		Effort struct {
			Open effort `json:"open"`
		} `json:"effort"`
	}
	type report struct {
		Data struct {
			Insights struct {
				Slices []slice `json:"slices"`
				Fields []struct {
					By string `json:"by"`
				} `json:"fields"`
				Effort struct {
					Open effort `json:"open"`
				} `json:"effort"`
			} `json:"insights"`
		} `json:"data"`
	}
	read := func(who string) (report, string) {
		t.Helper()
		c := context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who))
		rec := httptest.NewRecorder()
		projectController.Report(rec, httptest.NewRequest(http.MethodGet, "/project/report?projects="+w.project, http.NoBody).WithContext(c))
		var r report
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s's report: %d %s", who, rec.Code, rec.Body.String())
		}
		return r, rec.Body.String()
	}
	slices := func(r report, by string) map[string]slice {
		out := map[string]slice{}
		for _, s := range r.Data.Insights.Slices {
			if s.By == by {
				out[s.Key] = s
			}
		}
		return out
	}
	byCustomer, byBudget := "field:"+customer.ID.String(), "field:"+budget.ID.String()

	t.Run("a member: labels, cycle, estimate, text and effort, never a task kept from them", func(t *testing.T) {
		r, body := read("ben")
		if strings.Contains(strings.ToLower(body), "secret") {
			t.Fatalf("ben's report names the restricted task's label or text: %s", body)
		}
		for by, want := range map[string]map[string]int{
			"labels":   {"launch": 1, "copy": 1},
			"cycle":    {sprint.Id.String(): 1, "": 1},
			"estimate": {"5": 1, "": 1},
			"status":   {"todo": 2},
			byCustomer: {"acme": 2},
		} {
			got := map[string]int{}
			for k, s := range slices(r, by) {
				got[k] = s.Open
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("by %s: %v, want %v", by, got, want)
			}
		}
		if s := slices(r, "cycle")[sprint.Id.String()]; s.Name != "Sprint 1" || s.Effort.Open.Points != 5 {
			t.Errorf("the cycle's slice: %+v", s)
		}
		if s := slices(r, byCustomer)["acme"]; s.Name != "Acme" {
			t.Errorf("a text slice is named as first written: %+v", s)
		}
		if e := r.Data.Insights.Effort.Open; e.Points != 5 {
			t.Errorf("ben's open effort %+v, want 5 points", e)
		}
		// Money is for the project's admins: no Budget for a member.
		if len(slices(r, byBudget)) != 0 || strings.Contains(body, budget.ID.String()) {
			t.Errorf("a member slices by money: %s", body)
		}
	})

	t.Run("an admin: the restricted task's values, and money", func(t *testing.T) {
		r, _ := read("ana")
		if s, ok := slices(r, byCustomer)["secret corp"]; !ok || s.Open != 1 {
			t.Errorf("ana reads the restricted task's text: %+v", slices(r, byCustomer))
		}
		if _, ok := slices(r, "labels")["secret-plan"]; !ok {
			t.Errorf("ana reads the restricted task's label: %+v", slices(r, "labels"))
		}
		if s, ok := slices(r, byBudget)["500000"]; !ok || s.Name != "5000.00 USD" {
			t.Errorf("an admin slices by money: %+v", slices(r, byBudget))
		}
		if e := r.Data.Insights.Effort.Open; e.Points != 8 {
			t.Errorf("ana's open effort %+v, want 8 points", e)
		}
	})
}
