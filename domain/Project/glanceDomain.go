package Domain

// A project's glance (PR1, the BE audit's glance counts): open, done, overdue
// and due this week, counted by the graph over all of the project's tasks the
// reader may see. The glance line counted what the board had loaded (up to
// 1,000 a column), so a big project's numbers were "at least".

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// Glance is a project's counts.
type Glance struct {
	Open    int `json:"open"`
	Done    int `json:"done"`
	Overdue int `json:"overdue"`
	Week    int `json:"due_this_week"`
}

// GetProjectGlance counts a project's tasks for the reader of ctx: overdue is
// open and due before today began, due this week open and due from then for
// seven days (today begins at today, in the reader's zone).
func GetProjectGlance(ctx context.Context, projectUUID string, today time.Time) (*Glance, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	live, open := dgraphStruct.TASK_LIVE_FILTER, dgraphStruct.TASK_OPEN_FILTER
	query := tr.Fill(`query Glance($id: string, $today: string, $week: string) {
		g(func: eq(project_uuid, $id)) {
			open: count(project_tasks @filter(` + live + ` AND ` + open + ` @@hidden_tasks@@))
			done: count(project_tasks @filter(` + live + ` AND eq(task_status, "done") @@hidden_tasks@@))
			overdue: count(project_tasks @filter(` + live + ` AND ` + open + ` AND ` + dgraphquery.Deleted("task_due_date") + ` AND lt(task_due_date, $today) @@hidden_tasks@@))
			due_this_week: count(project_tasks @filter(` + live + ` AND ` + open + ` AND ge(task_due_date, $today) AND lt(task_due_date, $week) @@hidden_tasks@@))
		}
	}`)
	got, err := dgexec.List[Glance](ctx, query, map[string]string{
		"$id": projectUUID, "$today": today.UTC().Format(time.RFC3339), "$week": today.AddDate(0, 0, 7).UTC().Format(time.RFC3339),
	}, "g")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetProjectGlance err: %+v", err)
		return nil, err
	}
	if len(got) == 0 {
		return &Glance{}, nil
	}
	return &got[0], nil
}
