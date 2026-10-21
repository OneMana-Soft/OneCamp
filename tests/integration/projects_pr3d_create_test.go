//go:build integration

package integration_test

// PR3 (the BE audit's #30): many tasks are made at once with one statement
// for their rows and numbers, one graph write and one search write, not a
// whole CreateTask each: a whiteboard's notes (POST /task/createTasks) and a
// project started from a template cost the same for 5 tasks or 50.
// Run: integ_rel.sh <wt> <log> 'TestPR3dManyTasksAreMadeAtAFixedCost'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func pr3dCreate(t *testing.T, s *wa2Scale, who string, names []string) []string {
	t.Helper()
	ctx := context.Background()
	pg, _ := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people[who])
	gr, _ := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people[who].String())
	tasks := make([]map[string]string, len(names))
	for i, n := range names {
		tasks[i] = map[string]string{"task_name": n}
	}
	body, _ := json.Marshal(map[string]any{"project_uuid": s.project, "tasks": tasks})
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, chi.NewRouteContext())
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(http.HandlerFunc(taskBulkController.CreateTasks)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(c))
	var got struct {
		Data struct {
			Made []string `json:"made"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Data.Made) != len(names) {
		t.Fatalf("create %d: %d %s", len(names), rec.Code, rec.Body.String())
	}
	return got.Data.Made
}

func TestPR3dManyTasksAreMadeAtAFixedCost(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.exec(`INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'BIG', 1)`, s.project)
	names := func(n int, prefix string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s %d", prefix, i+1)
		}
		return out
	}
	pr3dCreate(t, s, "ben", names(1, "Warm")) // what is kept a while: policies, statuses

	// What a batch costs counts what it sets going after it answers too (its
	// events, its notice), once they have settled.
	settled := func() int {
		last := -1
		for n := counter.Count(); n != last; n = counter.Count() {
			last = n
			time.Sleep(300 * time.Millisecond)
		}
		return last
	}
	settled()
	counter.Reset()
	few := pr3dCreate(t, s, "ben", names(5, "Note"))
	fewCost := settled()
	counter.Reset()
	many := pr3dCreate(t, s, "ben", names(50, "Sticky"))
	manyCost := settled()
	t.Logf("Postgres statements to make 5 tasks: %d; 50: %d", fewCost, manyCost)
	if fewCost != manyCost {
		t.Fatalf("5 tasks cost %d statements and 50 cost %d: making many should cost the same", fewCost, manyCost)
	}

	// Numbered in order after the ones before, none skipped.
	var first, last, count int
	if err := env.PG.QueryRow(`SELECT min(task_number), max(task_number), count(*) FROM tasks WHERE id = ANY($1::uuid[])`, "{"+pr3dJoin(many)+"}").Scan(&first, &last, &count); err != nil {
		t.Fatal(err)
	}
	if count != 50 || first != 7 || last != 56 {
		t.Fatalf("numbers %d to %d over %d tasks, want 7 to 56 over 50", first, last, count)
	}
	var n0 int
	if err := env.PG.QueryRow(`SELECT task_number FROM tasks WHERE id = $1`, many[0]).Scan(&n0); err != nil || n0 != 7 {
		t.Fatalf("the first made is %d (%v), want 7", n0, err)
	}

	// In the graph, in order, in the project, each with its creation in its history.
	got := pr2Names(t, s, append(few, many...))
	for i, id := range many {
		if want := fmt.Sprintf("Sticky %d", i+1); got[id] != want {
			t.Fatalf("task %d is %q, want %q", i, got[id], want)
		}
	}
	var proj struct {
		P []struct {
			N int `json:"n"`
		} `json:"p"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ p(func: eq(project_uuid, "`+s.project+`")) { n: count(project_tasks) } }`), &proj); err != nil || len(proj.P) != 1 || proj.P[0].N != 56 {
		t.Fatalf("the project holds %+v tasks (%v), want 56", proj, err)
	}
	var hist struct {
		Q []struct {
			A []struct {
				Type string `json:"activity_type"`
			} `json:"task_activities"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(task_uuid, "`+many[0]+`")) { task_activities { activity_type } } }`), &hist); err != nil || len(hist.Q) != 1 || len(hist.Q[0].A) != 1 {
		t.Fatalf("the first task's history: %+v (%v)", hist, err)
	}

	// A template's tasks and their sub-tasks, made at once: the same cost for
	// 2 tasks as for 20, each sub-task under its task.
	apply := func(n int) int {
		t.Helper()
		ctx := context.Background()
		pg, _ := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people["ana"])
		gr, _ := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people["ana"].String())
		tpl := templateBusiness.Template{Name: "Launch"}
		for i := 0; i < n; i++ {
			tpl.Tasks = append(tpl.Tasks, templateBusiness.Task{Name: fmt.Sprintf("Step %d", i+1), Subtasks: []templateBusiness.Subtask{{Name: "Draft"}, {Name: "Review"}}})
		}
		user := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
		settled()
		counter.Reset()
		got := templateBusiness.ApplyTo(ctx, tpl, s.project, &user, templateBusiness.Start{Day: time.Now(), Loc: time.UTC})
		cost := settled()
		if got.Tasks != 3*n || got.Failed != 0 {
			t.Fatalf("applied %+v, want %d tasks", got, 3*n)
		}
		return cost
	}
	two, twenty := apply(2), apply(20)
	t.Logf("Postgres statements to apply a template of 2 tasks (6 with sub-tasks): %d; of 20 (60): %d", two, twenty)
	if two != twenty {
		t.Fatalf("a template of 2 tasks cost %d statements and of 20 cost %d", two, twenty)
	}
	var subs struct {
		P []struct {
			T []struct {
				Name string `json:"task_name"`
				N    int    `json:"n"`
			} `json:"project_tasks"`
		} `json:"p"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ p(func: eq(project_uuid, "`+s.project+`")) { project_tasks @filter(has(task_sub_tasks)) { task_name n: count(task_sub_tasks) } } }`), &subs); err != nil || len(subs.P) != 1 {
		t.Fatalf("the project's tasks with sub-tasks: %+v (%v)", subs, err)
	}
	found := false
	for _, task := range subs.P[0].T {
		if task.Name == "Step 20" {
			found = true
			if task.N != 2 {
				t.Fatalf("Step 20 has %d sub-tasks, want 2", task.N)
			}
		}
	}
	if !found {
		t.Fatalf("no Step 20 with sub-tasks: %+v", subs)
	}
}

func pr3dJoin(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
