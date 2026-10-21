//go:build integration

package integration_test

// The backlog (B3, STEP4_PLAN §4 #9): a project's open cycles with their
// tasks, then its open top-level tasks in no open cycle, counted and totalled
// over all of them, for the reader (a restricted task they can't see counts
// nowhere), in a fixed number of Postgres statements however many tasks; and
// starting the next cycle.
// Run: integ_rel.sh <wt> <log> 'TestB3'

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type b3Answer struct {
	Data struct {
		Backlog struct {
			Cycles []struct {
				ID       string `json:"id"`
				Number   int    `json:"number"`
				State    string `json:"state"`
				Estimate int    `json:"estimate_minutes"`
				Tasks    []struct {
					UUID string `json:"task_uuid"`
				} `json:"tasks"`
			} `json:"cycles"`
			Tasks []struct {
				UUID       string `json:"task_uuid"`
				Restricted bool   `json:"task_restricted"`
			} `json:"tasks"`
			Count    int  `json:"count"`
			Estimate int  `json:"estimate_minutes"`
			Capacity *int `json:"capacity_minutes"`
		} `json:"backlog"`
		CanEdit bool `json:"can_edit"`
		CanPlan bool `json:"can_plan"`
	} `json:"data"`
}

func b3Call(s *wa2Scale, who, method string, h http.HandlerFunc, cycle string) (int, string) {
	s.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people[who])
	if err != nil || pg == nil {
		s.t.Fatalf("load %s: %v", who, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people[who].String())
	if err != nil || gr == nil {
		s.t.Fatalf("load %s's profile: %v", who, err)
	}
	rc := chi.NewRouteContext()
	rc.URLParams.Add("project_uuid", s.project)
	rc.URLParams.Add("cycle_id", cycle)
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(method, "/", nil).WithContext(c))
	return rec.Code, rec.Body.String()
}

func b3Backlog(t *testing.T, s *wa2Scale, who string) b3Answer {
	t.Helper()
	code, out := b3Call(s, who, http.MethodGet, cycleController.Backlog, "")
	if code != http.StatusOK {
		t.Fatalf("the backlog as %s: %d %s", who, code, out)
	}
	var a b3Answer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestB3BacklogIsTheOpenCyclesThenEverythingInNone(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(60) // statuses cycle todo, inProgress, inReview, backlog, done, canceled
	project := mustUUID(t, s.project)
	open := func(i int) bool { return i%6 < 4 }

	// An estimate on every task: (i+1) * 10 minutes.
	var est []map[string]any
	for i, id := range s.tasks {
		est = append(est, map[string]any{"uid": uidOfTask(t, s, id), "task_estimate_minutes": (i + 1) * 10})
	}
	dg.Mutate(t, est)

	now := time.Now().UTC()
	old, err := cycleModel.Create(project, "", now.AddDate(0, 0, -28), now.AddDate(0, 0, -14), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	cur, err := cycleModel.Create(project, "", now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	next, err := cycleModel.Create(project, "", now.AddDate(0, 0, 11), now.AddDate(0, 0, 25), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	// The old cycle finished tasks 4 and 10 (done): 50 + 110 minutes.
	for _, i := range []int{4, 10} {
		if err := cycleModel.SetTask(mustUUID(t, s.tasks[i]), &old.Id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cycleModel.Complete(old.Id, 2, nil, nil); err != nil {
		t.Fatal(err)
	}
	inCur, inNext := []int{0, 1, 16}, []int{2, 3} // 16 is done
	for _, i := range inCur {
		if err := cycleModel.SetTask(mustUUID(t, s.tasks[i]), &cur.Id); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range inNext {
		if err := cycleModel.SetTask(mustUUID(t, s.tasks[i]), &next.Id); err != nil {
			t.Fatal(err)
		}
	}
	planned := map[int]bool{0: true, 1: true, 16: true, 2: true, 3: true}
	wantCount, wantMinutes := 0, 0
	for i := range s.tasks {
		if open(i) && !planned[i] {
			wantCount++
			wantMinutes += (i + 1) * 10
		}
	}

	a := b3Backlog(t, s, "ana")
	b := a.Data.Backlog
	if !a.Data.CanEdit || !a.Data.CanPlan {
		t.Fatalf("the project's admin edits and plans: %+v", a.Data)
	}
	if len(b.Cycles) != 2 || b.Cycles[0].ID != cur.Id.String() || b.Cycles[0].State != "current" || b.Cycles[1].ID != next.Id.String() {
		t.Fatalf("the open cycles, current first: %+v", b.Cycles)
	}
	if len(b.Cycles[0].Tasks) != 3 || b.Cycles[0].Estimate != 10+20+170 || len(b.Cycles[1].Tasks) != 2 || b.Cycles[1].Estimate != 30+40 {
		t.Fatalf("each cycle's tasks and estimate: %+v", b.Cycles)
	}
	if b.Count != wantCount || b.Estimate != wantMinutes || len(b.Tasks) != wantCount {
		t.Fatalf("the backlog: %d tasks, %d minutes (%d shown), want %d and %d", b.Count, b.Estimate, len(b.Tasks), wantCount, wantMinutes)
	}
	for _, x := range b.Tasks {
		for i := range planned {
			if x.UUID == s.tasks[i] {
				t.Fatalf("a planned task is in the backlog: %s", x.UUID)
			}
		}
	}
	// 160 minutes is 2.7 h to a tenth, as velocity says it.
	if b.Capacity == nil || *b.Capacity != 162 {
		t.Fatalf("capacity is what the completed cycle finished: %v", b.Capacity)
	}

	t.Run("a member may plan but not run cycles, and doesn't count what they can't see", func(t *testing.T) {
		// Task 6 is open, in the backlog, and restricted (restrict keeps every sixth from 2: 2, 8, 14).
		if !s.restrict(3) {
			t.Fatal("no task_restrictions table")
		}
		defer s.restrict(0)
		m := b3Backlog(t, s, "ben")
		hidden, hiddenMinutes := 0, 0
		for _, i := range []int{8, 14} { // 2 is in the next cycle
			if open(i) {
				hidden++
				hiddenMinutes += (i + 1) * 10
			}
		}
		if m.Data.CanEdit || !m.Data.CanPlan {
			t.Fatalf("a member: %+v", m.Data)
		}
		if mb := m.Data.Backlog; mb.Count != wantCount-hidden || mb.Estimate != wantMinutes-hiddenMinutes || len(mb.Cycles[1].Tasks) != 1 {
			t.Fatalf("ben's backlog: %d tasks, %d minutes, next cycle %d tasks", mb.Count, mb.Estimate, len(mb.Cycles[1].Tasks))
		}
		// Ana sees them, with the lock.
		locks := 0
		for _, x := range b3Backlog(t, s, "ana").Data.Backlog.Tasks {
			if x.Restricted {
				locks++
			}
		}
		if locks != hidden {
			t.Fatalf("ana's backlog marks %d restricted, want %d", locks, hidden)
		}
	})

	t.Run("its Postgres statements don't grow with its tasks", func(t *testing.T) {
		count := func() int {
			b3Backlog(t, s, "ben")
			counter.Reset()
			b3Backlog(t, s, "ben")
			return counter.Count()
		}
		few := count()
		s.addTasks(540)
		many := count()
		t.Logf("the backlog sends Postgres %d statements with 60 tasks, %d with 600", few, many)
		if many != few || many > 10 {
			t.Fatalf("%d statements for 600 tasks, %d for 60 (at most 10)", many, few)
		}
	})

	t.Run("the next cycle starts when the current one is complete", func(t *testing.T) {
		if code, out := b3Call(s, "ben", http.MethodPost, cycleController.StartCycle, next.Id.String()); code != http.StatusForbidden {
			t.Fatalf("a member starts a cycle: %d %s", code, out)
		}
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.StartCycle, next.Id.String()); code != http.StatusBadRequest || !json.Valid([]byte(out)) {
			t.Fatalf("with the current one open: %d %s", code, out)
		}
		if _, err := cycleModel.Complete(cur.Id, 1, nil, nil); err != nil {
			t.Fatal(err)
		}
		code, out := b3Call(s, "ana", http.MethodPost, cycleController.StartCycle, next.Id.String())
		if code != http.StatusOK {
			t.Fatalf("start: %d %s", code, out)
		}
		got, err := cycleModel.Get(next.Id)
		if err != nil || time.Since(got.StartsAt) > time.Minute || got.EndsAt.Sub(got.StartsAt) != 14*24*time.Hour {
			t.Fatalf("starts now, as long as planned: %+v %v", got, err)
		}
		done, _ := cycleModel.Get(cur.Id)
		if done.EndsAt.After(got.StartsAt) {
			t.Fatalf("the completed cycle ends when the next starts: %v after %v", done.EndsAt, got.StartsAt)
		}
		if code, _ := b3Call(s, "ana", http.MethodPost, cycleController.StartCycle, next.Id.String()); code != http.StatusBadRequest {
			t.Fatalf("starting it again: %d", code)
		}
	})
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// uidOfTask is the graph's uid of the task with id.
func uidOfTask(t *testing.T, s *wa2Scale, id string) string {
	t.Helper()
	var out struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(s.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { uid } }`, id)), &out); err != nil || len(out.Q) != 1 {
		t.Fatalf("task %s: %+v %v", id, out, err)
	}
	return out.Q[0].UID
}
