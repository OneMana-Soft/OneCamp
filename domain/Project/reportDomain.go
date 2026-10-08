package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ReportProjectsCap is how many tasks of each kind (open, and closed since
// the report's first week) the report reads from one project.
const ReportProjectsCap = 5000

// ReportProject is one of a person's live projects as the report reads it:
// its open tasks, and the ones that closed (done or cancelled) since a day,
// the latest first. Subtasks count, as the overview counts them.
type ReportProject struct {
	UUID   string                     `json:"project_uuid"`
	Name   string                     `json:"project_name"`
	Open   []*dgraphStruct.DgraphTask `json:"open"`
	Closed []*dgraphStruct.DgraphTask `json:"closed"`
	// Truncated says a project had more of either than ReportProjectsCap.
	Truncated bool `json:"-"`
}

// reportPerson is what the report shows of an assignee, and what says whether
// they count: a bot or a deleted account doesn't.
const reportPerson = `uid user_uuid user_name user_full_name user_profile_object_key user_deleted_at is_bot`

// reportTask is what the report reads of a task, its history included for the
// flow of work (activity_type has no index to filter on, so the status
// changes are picked out in Go). (With its uid: Dgraph leaves out a node with
// nothing to show, which an unassigned one could be.)
const reportTask = `uid task_status task_priority task_due_date task_created_at task_status_since task_assignee { ` + reportPerson + ` }
	task_activities { activity_type activity_time activity_prev_state activity_next_state }`

// GetDgraphReport reads, for every live project the person is in, its open
// live tasks and the live tasks that closed since since.
func GetDgraphReport(ctx context.Context, userDgraphUID string, since time.Time) ([]ReportProject, error) {
	const epoch = `"1970-01-01T00:00:00Z"`
	closed := fmt.Sprintf(`(eq(task_status, %q) OR eq(task_status, %q)) AND ge(task_status_since, $since)`,
		dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_CANCELED)
	query := fmt.Sprintf(`query Report($user: string, $since: string) {
		me(func: uid($user)) {
			user_projects @filter(not gt(project_deleted_at, %[1]s)) {
				project_uuid
				project_name
				open: project_tasks @filter(%[2]s AND %[3]s) (first: %[5]d) { %[6]s }
				closed: project_tasks @filter(%[2]s AND %[4]s) (orderdesc: task_status_since, first: %[5]d) { %[6]s }
			}
		}
	}`, epoch, dgraphStruct.TASK_LIVE_FILTER, dgraphStruct.TASK_OPEN_FILTER, closed, ReportProjectsCap+1, reportTask)
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{
		"$user": userDgraphUID, "$since": since.UTC().Format(time.RFC3339),
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphReport err: %+v", err)
		return nil, err
	}
	var out struct {
		Me []struct {
			Projects []ReportProject `json:"user_projects"`
		} `json:"me"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	if len(out.Me) == 0 {
		return []ReportProject{}, nil
	}
	projects := out.Me[0].Projects
	for i := range projects {
		p := &projects[i]
		for _, list := range []*[]*dgraphStruct.DgraphTask{&p.Open, &p.Closed} {
			if len(*list) > ReportProjectsCap {
				*list, p.Truncated = (*list)[:ReportProjectsCap], true
			}
		}
	}
	return projects, nil
}
