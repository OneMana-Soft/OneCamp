//go:build integration

package business

// The projects overview against a real Dgraph (counts) and Postgres 12 with
// every migration (updates).
// Run: go test -tags=integration ./business/ProjectUpdate/ -run TestOverviews -v

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestOverviews(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", "2026-03-01T00:00:00Z"
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	task := func(status, due string, deleted bool) map[string]any {
		m := map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_status": status, "task_deleted_at": live}
		if due != "" {
			m["task_due_date"] = due
		}
		if deleted {
			m["task_deleted_at"] = gone
		}
		return m
	}
	withStart := func(m map[string]any, start string) map[string]any {
		m["task_start_date"] = start
		return m
	}
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": uuid.NewString(),
		"user_projects": []map[string]any{
			{"uid": "_:a", "dgraph.type": "Project", "project_uuid": a.String(), "project_name": "beta launch", "project_deleted_at": live,
				"project_admins": []map[string]any{{"uid": "_:me"}},
				"project_tasks": []map[string]any{
					task("todo", "2026-03-09T17:00:00Z", false),                   // overdue
					task("inProgress", "2026-03-12T17:00:00Z", false),             // due this week
					task("todo", "2026-03-20T17:00:00Z", false),                   // open, later
					withStart(task("backlog", "", false), "2026-02-25T09:00:00Z"), // open, no due date; the first to start
					task("done", "2026-03-01T17:00:00Z", false),                   // done, never overdue
					task("canceled", "2026-03-01T17:00:00Z", false),               // neither
					task("todo", "2026-03-09T17:00:00Z", true),                    // deleted
				}},
			{"uid": "_:b", "dgraph.type": "Project", "project_uuid": b.String(), "project_name": "Alpha", "project_deleted_at": live},
			{"uid": "_:c", "dgraph.type": "Project", "project_uuid": c.String(), "project_name": "Archived", "project_deleted_at": gone},
		},
	})
	author := uuid.New()
	if _, err := model.Add(a, author, model.OnTrack, "older", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := model.Add(a, author, model.AtRisk, "newer", false); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	list, err := Overviews(ctx, uids["me"], now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "beta launch" {
		t.Fatalf("live projects only, by name whatever the case: %+v", list)
	}
	alpha, beta := list[0], list[1]
	if alpha.Open != 0 || alpha.Done != 0 || alpha.Health != "" || alpha.UpdatedAt != nil || alpha.IsAdmin != 0 {
		t.Errorf("a project with nothing yet: %+v", alpha)
	}
	if beta.Open != 4 || beta.Done != 1 || beta.Overdue != 1 || beta.DueSoon != 1 || beta.IsAdmin != 1 {
		t.Errorf("counts as the project page counts them: %+v", beta.ProjectCounts)
	}
	if beta.Health != model.AtRisk || beta.UpdatedAt == nil {
		t.Errorf("health from the newest update: %q %v", beta.Health, beta.UpdatedAt)
	}
	if beta.Team != nil {
		t.Errorf("no team in this graph: %+v", beta.Team)
	}
	// Its tasks run from the first start (the backlog task's) to the last
	// due date; the deleted task's dates don't count, and an empty project has none.
	if beta.FirstDay == nil || !beta.FirstDay.Equal(time.Date(2026, 2, 25, 9, 0, 0, 0, time.UTC)) ||
		beta.LastDay == nil || !beta.LastDay.Equal(time.Date(2026, 3, 20, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("the days its tasks run across: %v to %v", beta.FirstDay, beta.LastDay)
	}
	if alpha.FirstDay != nil || alpha.LastDay != nil {
		t.Errorf("a project with no dated tasks has no days: %v %v", alpha.FirstDay, alpha.LastDay)
	}

	// In a zone where it's already the 11th, the task due on the 12th is
	// still this week and the one due on the 9th is still overdue.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	list, err = Overviews(ctx, uids["me"], time.Date(2026, 3, 11, 1, 0, 0, 0, tokyo))
	if err != nil || list[1].Overdue != 1 || list[1].DueSoon != 1 {
		t.Fatalf("days are the person's: %+v %v", list, err)
	}
}
