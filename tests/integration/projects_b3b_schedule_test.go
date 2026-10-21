//go:build integration

package integration_test

// B3b, the Cycle Scheduler: a project's cycles run themselves. Saving a
// schedule makes the current cycle and the next ones; the Sweeper's pass
// completes a cycle when it ends, rolling its unfinished tasks into the next,
// and keeps the next ones ready; a pass reads the same few times however many
// projects are scheduled.
// Run: integ_rel.sh <wt> <log> 'TestB3bTheSchedulerRunsAProjectsCycles'

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
	"github.com/google/uuid"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// postAs runs a handler as who with a JSON body and the project's id as a
// route parameter.
func (s *wa2Scale) postAs(who string, h http.HandlerFunc, body any) (int, string) {
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
	raw, _ := json.Marshal(body)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("project_uuid", s.project)
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
	return rec.Code, rec.Body.String()
}

type b3bCycle struct {
	ID          string     `json:"id"`
	Number      int        `json:"number"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
	CompletedAt *time.Time `json:"completed_at"`
	State       string     `json:"state"`
}

func (s *wa2Scale) cycles(who string) ([]b3bCycle, bool) {
	s.t.Helper()
	code, out := s.read(who, cycleController.ListCycles, "")
	if code != http.StatusOK {
		s.t.Fatalf("cycles: %d %s", code, out)
	}
	var got struct {
		Data struct {
			Cycles   []b3bCycle      `json:"cycles"`
			Schedule json.RawMessage `json:"schedule"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		s.t.Fatal(err)
	}
	return got.Data.Cycles, string(got.Data.Schedule) != "null" && len(got.Data.Schedule) > 0
}

func TestB3bTheSchedulerRunsAProjectsCycles(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(6) // statuses go round todo, inProgress, inReview, backlog, done, canceled
	ctx := context.Background()

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	in := map[string]any{"weeks": 1, "cooldown_weeks": 0, "upcoming": 2, "roll_over": true, "starts_at": today.AddDate(0, 0, -3)}

	// Only the project's admins set it.
	if code, out := s.postAs("ben", cycleController.SetCycleSchedule, in); code != http.StatusForbidden {
		t.Fatalf("a member set the schedule: %d %s", code, out)
	}
	if code, out := s.postAs("ana", cycleController.SetCycleSchedule, map[string]any{"weeks": 9, "upcoming": 2, "starts_at": today}); code != http.StatusBadRequest {
		t.Fatalf("nine weeks: %d %s", code, out)
	}
	if code, out := s.postAs("ana", cycleController.SetCycleSchedule, in); code != http.StatusOK {
		t.Fatalf("set the schedule: %d %s", code, out)
	}

	// Saving it made the cycle running now and the next two.
	cycles, scheduled := s.cycles("ben")
	if !scheduled || len(cycles) != 3 {
		t.Fatalf("after saving: scheduled %v, cycles %+v", scheduled, cycles)
	}
	current := cycles[0]
	if current.State != "current" || !current.StartsAt.Equal(today.AddDate(0, 0, -3)) || !current.EndsAt.Equal(today.AddDate(0, 0, 4)) {
		t.Fatalf("the current cycle: %+v", current)
	}
	for i, c := range cycles[1:] {
		if c.State != "upcoming" || !c.StartsAt.Equal(cycles[i].EndsAt) {
			t.Fatalf("upcoming cycle %d: %+v after %+v", i+1, c, cycles[i])
		}
	}

	// An open task and a done one in the running cycle.
	open, done := s.tasks[0], s.tasks[4]
	for _, task := range []string{open, done} {
		s.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, task, current.ID)
	}

	// A pass after it ends completes it; the open task rolls into the next;
	// the next two are kept ready.
	later := today.AddDate(0, 0, 5)
	if err := cycleBusiness.RunSchedules(ctx, later); err != nil {
		t.Fatal(err)
	}
	cycles, _ = s.cycles("ben")
	if len(cycles) != 4 || cycles[0].CompletedAt == nil {
		t.Fatalf("after it ended: %+v", cycles)
	}
	var in2 string
	if err := env.PG.QueryRow(`SELECT cycle_id::text FROM task_cycles WHERE task_uuid = $1`, open).Scan(&in2); err != nil || in2 != cycles[1].ID {
		t.Fatalf("the open task is in %q (%v), want the next cycle %s", in2, err, cycles[1].ID)
	}
	var doneStays int
	if err := env.PG.QueryRow(`SELECT count(*) FROM task_cycles WHERE task_uuid = $1 AND cycle_id = $2`, done, current.ID).Scan(&doneStays); err != nil || doneStays != 1 {
		t.Fatalf("the done task left the completed cycle: %d %v", doneStays, err)
	}

	// A pass with nothing to do reads the same few times for one project or
	// six (each with its schedule, all ready).
	quiet := func() int {
		t.Helper()
		if err := cycleBusiness.RunSchedules(ctx, later); err != nil {
			t.Fatal(err)
		}
		counter.Reset()
		if err := cycleBusiness.RunSchedules(ctx, later); err != nil {
			t.Fatal(err)
		}
		return counter.Count()
	}
	one := quiet()
	for i := 0; i < 5; i++ {
		p := uuid.New()
		s.exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, p, fmt.Sprintf("Scheduled %d", i), s.people["ana"])
		dg.Mutate(t, []map[string]any{{"uid": "_:p", "dgraph.type": "Project", "project_uuid": p.String(), "project_name": fmt.Sprintf("Scheduled %d", i), "project_deleted_at": "0001-01-01T00:00:00Z"}})
		s.exec(`INSERT INTO cycle_schedules (project_uuid, weeks, upcoming, anchor) VALUES ($1, 2, 2, $2)`, p, today)
	}
	six := quiet()
	t.Logf("Postgres statements for a pass with nothing to do: %d for one project, %d for six", one, six)
	if one != six || one > 2 {
		t.Fatalf("a quiet pass sends %d statements for one project and %d for six; want the same, at most 2", one, six)
	}

	// Stopped, the project's cycles stay and no more are made.
	if code, out := s.postAs("ana", cycleController.StopCycleSchedule, nil); code != http.StatusOK {
		t.Fatalf("stop: %d %s", code, out)
	}
	if err := cycleBusiness.RunSchedules(ctx, later.AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	after, scheduled := s.cycles("ben")
	if scheduled || len(after) != 4 || after[1].CompletedAt != nil {
		t.Fatalf("after stopping: scheduled %v, %+v", scheduled, after)
	}
}
