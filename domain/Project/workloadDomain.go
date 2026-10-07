package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// WorkloadProjectsCap is how many dated tasks the workload reads from one
// project, the latest due first: an import can leave thousands of old open
// tasks, and they mustn't crowd out this quarter's.
const WorkloadProjectsCap = 3000

// WorkloadProject is one of a person's projects as the workload reads it:
// its people (members and admins, each with their weekly capacity), its open
// tasks that have dates and start by the end of the weeks shown, and who has
// the open tasks without dates.
type WorkloadProject struct {
	UUID    string                     `json:"project_uuid"`
	Name    string                     `json:"project_name"`
	IsAdmin int                        `json:"is_admin"`
	Members []*dgraphStruct.DgraphUser `json:"project_members"`
	Admins  []*dgraphStruct.DgraphUser `json:"project_admins"`
	Dated   []*dgraphStruct.DgraphTask `json:"dated"`
	Undated []*dgraphStruct.DgraphTask `json:"undated"`
	// Truncated says more dated tasks were there than WorkloadProjectsCap.
	Truncated bool `json:"-"`

	DatedStart []*dgraphStruct.DgraphTask `json:"dated_start"`
}

// workloadPerson is what the workload shows of a person, and what says
// whether they count: a bot or a deleted account doesn't.
const workloadPerson = `uid user_uuid user_name user_full_name user_profile_object_key user_job_title user_deleted_at is_bot user_weekly_capacity`

// GetDgraphWorkload reads the workload of every live project the person is
// in: open, live tasks (subtasks too, as the overview counts them) whose
// first day (their start, else their due date) is before until, and the
// assignees of those with no dates at all. (Each task is read with its uid:
// Dgraph leaves out a node with nothing to show, which an unassigned one is.)
func GetDgraphWorkload(ctx context.Context, userDgraphUID string, until time.Time) ([]WorkloadProject, error) {
	const epoch = `"1970-01-01T00:00:00Z"`
	open := dgraphStruct.TASK_LIVE_FILTER + ` AND ` + dgraphStruct.TASK_OPEN_FILTER
	// Tasks with a due date, the latest due first, and tasks with only a start,
	// the latest first, each capped on its own: one sort would put the
	// start-only tasks last and cut them before tasks overdue for years.
	startsBy := `(gt(task_start_date, ` + epoch + `) AND lt(task_start_date, $until)) OR ` +
		`(NOT gt(task_start_date, ` + epoch + `) AND lt(task_due_date, $until))`
	withDue := `gt(task_due_date, ` + epoch + `) AND (` + startsBy + `)`
	startOnly := `NOT gt(task_due_date, ` + epoch + `) AND gt(task_start_date, ` + epoch + `) AND lt(task_start_date, $until)`
	const taskFields = `uid task_uuid task_name task_status task_custom_status task_custom_status_name task_start_date task_due_date task_parent_task { task_uuid task_name }`
	query := fmt.Sprintf(`query Workload($user: string, $until: string) {
		me(func: uid($user)) {
			user_projects @filter(not gt(project_deleted_at, %[1]s)) {
				project_uuid
				project_name
				is_admin: count(project_admins @filter(uid($user)))
				project_members { %[2]s }
				project_admins { %[2]s }
				dated: project_tasks @filter(%[3]s AND %[4]s) (orderdesc: task_due_date, first: %[5]d) {
					%[7]s
					task_assignee { %[2]s }
				}
				dated_start: project_tasks @filter(%[3]s AND %[6]s) (orderdesc: task_start_date, first: %[5]d) {
					%[7]s
					task_assignee { %[2]s }
				}
				undated: project_tasks @filter(%[3]s AND NOT gt(task_start_date, %[1]s) AND NOT gt(task_due_date, %[1]s)) {
					uid
					task_assignee { %[2]s }
				}
			}
		}
	}`, epoch, workloadPerson, open, withDue, WorkloadProjectsCap+1, startOnly, taskFields)
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{
		"$user": userDgraphUID, "$until": until.UTC().Format(time.RFC3339),
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphWorkload err: %+v", err)
		return nil, err
	}
	var out struct {
		Me []struct {
			Projects []WorkloadProject `json:"user_projects"`
		} `json:"me"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	if len(out.Me) == 0 {
		return []WorkloadProject{}, nil
	}
	projects := out.Me[0].Projects
	for i := range projects {
		p := &projects[i]
		for _, list := range []*[]*dgraphStruct.DgraphTask{&p.Dated, &p.DatedStart} {
			if len(*list) > WorkloadProjectsCap {
				*list, p.Truncated = (*list)[:WorkloadProjectsCap], true
			}
		}
		p.Dated, p.DatedStart = append(p.Dated, p.DatedStart...), nil
	}
	return projects, nil
}

// SetDgraphWeeklyCapacity sets how many tasks a week the person takes on, or
// with tasks 0 clears it, so the default applies. It reports whether the
// person exists.
func SetDgraphWeeklyCapacity(ctx context.Context, userUUID string, tasks int) (bool, error) {
	nquad := fmt.Sprintf(`uid(u) <user_weekly_capacity> "%d" .`, tasks)
	mu := &api.Mutation{Cond: "@if(eq(len(u), 1))", SetNquads: []byte(nquad)}
	if tasks == 0 {
		mu = &api.Mutation{Cond: "@if(eq(len(u), 1))", DelNquads: []byte(`uid(u) <user_weekly_capacity> * .`)}
	}
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     `query q($id: string) { found(func: eq(user_uuid, $id)) { u as uid } }`,
		Vars:      map[string]string{"$id": userUUID},
		Mutations: []*api.Mutation{mu},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetDgraphWeeklyCapacity err: %+v", err)
		return false, err
	}
	var out struct {
		Found []struct{} `json:"found"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return false, err
	}
	return len(out.Found) == 1, nil
}
