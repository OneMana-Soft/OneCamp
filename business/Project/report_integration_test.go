//go:build integration

package business

// The report's reads against a real Dgraph with the production schema, and
// Postgres 12 with every migration.
// Run: go test -tags=integration ./business/Project/ -run 'TestReport' -v

import (
	"context"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	timeModels "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestReportQuery(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", "2026-09-01T00:00:00Z"
	task := func(name, status string, more map[string]any) map[string]any {
		m := map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": name, "task_status": status, "task_deleted_at": live}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	launch := uuid.NewString()
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "Me",
		"user_projects": []map[string]any{
			{
				"uid": "_:p", "dgraph.type": "Project", "project_uuid": launch, "project_name": "Launch",
				"project_tasks": []map[string]any{
					task("Open", "todo", map[string]any{"task_due_date": "2026-10-01T00:00:00Z", "task_assignee": map[string]any{"uid": "_:me"}}),
					task("Reviewing", "inReview", nil),
					task("Shipped this week", "done", map[string]any{"task_status_since": "2026-10-06T10:00:00Z", "task_created_at": "2026-09-20T10:00:00Z",
						"task_activities": []map[string]any{
							{"dgraph.type": "Activity", "activity_type": "statusUpdate", "activity_time": "2026-10-06T10:00:00Z", "activity_prev_state": "inProgress", "activity_next_state": "done"},
							{"dgraph.type": "Activity", "activity_type": "comment", "activity_time": "2026-10-05T10:00:00Z"},
						}}),
					task("Dropped", "canceled", map[string]any{"task_status_since": "2026-10-02T10:00:00Z"}),
					task("Shipped long ago", "done", map[string]any{"task_status_since": "2026-06-01T10:00:00Z"}),
					task("Deleted", "todo", map[string]any{"task_deleted_at": gone}),
				},
			},
			{
				"uid": "_:r", "dgraph.type": "Project", "project_uuid": uuid.NewString(), "project_name": "Archived", "project_deleted_at": gone,
				"project_tasks": []map[string]any{task("Old", "todo", nil)},
			},
		},
	})
	since := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	projects, err := domain.GetDgraphReport(ctx, uids["me"], since)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].UUID != launch {
		t.Fatalf("only the live project: %+v", projects)
	}
	p := projects[0]
	if len(p.Open) != 2 || len(p.Closed) != 2 {
		t.Fatalf("2 open (not the deleted one) and 2 closed since (not the old one): %d open, %d closed", len(p.Open), len(p.Closed))
	}
	if p.Closed[0].StatusSince == nil || !p.Closed[0].StatusSince.Equal(time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("latest closed first: %+v", p.Closed[0].StatusSince)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := BuildReport(projects, nil, now, time.UTC, 4, nil)
	// Made on Sunday 20 September: the first week's (from Monday the 14th).
	if r.Open != 2 || r.Overdue != 1 || r.DoneTotal != 1 || r.Done[3] != 1 || r.Added[0] != 1 {
		t.Fatalf("the report: open %d overdue %d done %v added %v", r.Open, r.Overdue, r.Done, r.Added)
	}
	// The flow of work reads the task's status change from its history: in
	// progress from when it was made, done in the last week.
	flow := buildFlow(projects, nil, since, now, 4, nil)
	if flow[1].InProgress != 1 || flow[3].Done != 1 || flow[3].InProgress != 0 {
		t.Fatalf("the flow: %+v", flow)
	}
}

func TestReportHoursByWeek(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, other, task, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, user); err != nil {
		t.Fatal(err)
	}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	add := func(p uuid.UUID, start time.Time, d time.Duration) {
		end := start.Add(d)
		if _, err := timeModels.Add(timeModels.Entry{TaskUUID: task, ProjectUUID: p, UserID: user, StartedAt: start, EndedAt: &end}); err != nil {
			t.Fatal(err)
		}
	}
	// Monday 5 October 01:00 in India is still Sunday in UTC: it counts in
	// the week of the 5th for someone in India.
	add(project, time.Date(2026, 10, 5, 1, 0, 0, 0, ist), time.Hour)
	add(project, time.Date(2026, 9, 30, 15, 0, 0, 0, ist), 90*time.Minute)
	add(other, time.Date(2026, 9, 30, 15, 0, 0, 0, ist), time.Hour)
	add(project, time.Date(2026, 8, 3, 15, 0, 0, 0, ist), time.Hour)
	// A running timer counts up to now.
	if _, _, err := timeModels.Start(user, task, project, time.Date(2026, 10, 8, 10, 0, 0, 0, ist)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 10, 30, 0, 0, ist)
	from := time.Date(2026, 9, 28, 0, 0, 0, 0, ist)
	rows, err := timeModels.SecondsByWeek([]uuid.UUID{project}, from, now, ist.String())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Week.Format(time.DateOnly)] += r.Seconds
	}
	if got["2026-09-28"] != 5400 || got["2026-10-05"] != 3600+1800 || len(got) != 2 {
		t.Fatalf("seconds by week %v, want 28 Sep 5400 and 5 Oct 5400", got)
	}
}
