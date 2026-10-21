package Domain

// The backlog's read (B3, STEP4_PLAN §4 #9): a project's open top-level
// tasks in no open cycle, a page of them with their count and estimate
// totalled over all of them by the graph (never from the page, PJ10), and
// the tasks of its open cycles and of its latest completed ones, in one query.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// BacklogPage is the most backlog tasks one read shows; the count and the
// estimate cover them all.
const BacklogPage = 300

// BacklogRead is what the backlog's one query answers.
type BacklogRead struct {
	// Backlog is a page of the open top-level tasks in no open cycle, oldest
	// first.
	Backlog []*dgraphStruct.DgraphTask
	// Count and Minutes are all of them, not the page.
	Count, Minutes int
	// Planned are the live tasks of planned (the open cycles' and the latest
	// completed ones'), as the reader may see them.
	Planned []*dgraphStruct.DgraphTask
}

var backlogTaskFields = `
	task_uuid
	task_name
	task_status
	task_custom_status
	task_custom_status_name
	task_priority
	task_due_date
	task_status_since
	task_created_at
	task_estimate_minutes
	task_assignee { uid user_uuid user_name user_full_name user_profile_object_key }
	task_sub_task_count: count(task_sub_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` @@hidden_tasks@@))
	task_sub_task_done: count(task_sub_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND (eq(task_status, "done") OR eq(task_status, "canceled")) @@hidden_tasks@@))
`

// uuidList is ids as a graph function's argument, each a parsed uuid, so
// nothing from outside reaches the query.
func uuidList(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if p, err := uuid.Parse(id); err == nil {
			out = append(out, p.String())
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// GetDgraphProjectBacklog reads project's backlog, leaving out the tasks in
// open cycles (inCycles) and reading those of planned, for the reader of ctx
// (models/taskread): one graph query however many tasks.
func GetDgraphProjectBacklog(ctx context.Context, projectUUID string, inCycles, planned []string) (*BacklogRead, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	notPlanned := ""
	if list := uuidList(inCycles); list != "[]" {
		notPlanned = " AND NOT eq(task_uuid, " + list + ")"
	}
	plannedBlock := ""
	if list := uuidList(planned); list != "[]" {
		plannedBlock = `planned(func: eq(task_uuid, ` + list + `)) @filter(` + dgraphquery.Live("task_deleted_at") + ` @@hidden_tasks@@) {` + backlogTaskFields + `}`
	}
	query := tr.Fill(fmt.Sprintf(`query Backlog($id: string) {
		var(func: eq(project_uuid, $id)) {
			project_tasks @filter(%s AND NOT has(task_parent_task) AND %s%s @@hidden_tasks@@) {
				b as uid
				e as task_estimate_minutes
			}
		}
		counted(func: uid(b)) { n: count(uid) }
		summed() { minutes: sum(val(e)) }
		backlog(func: uid(b), orderasc: task_created_at, first: %d) {%s}
		%s
	}`, dgraphStruct.TASK_LIVE_FILTER, dgraphStruct.TASK_OPEN_FILTER, notPlanned, BacklogPage, backlogTaskFields, plannedBlock))
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{"$id": projectUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphProjectBacklog err: %+v", err)
		return nil, err
	}
	var out struct {
		Counted []struct {
			N int `json:"n"`
		} `json:"counted"`
		Summed []struct {
			Minutes float64 `json:"minutes"`
		} `json:"summed"`
		Backlog []*dgraphStruct.DgraphTask `json:"backlog"`
		Planned []*dgraphStruct.DgraphTask `json:"planned"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	read := &BacklogRead{Backlog: tr.KeepTasks(out.Backlog), Planned: tr.KeepTasks(out.Planned)}
	if len(out.Counted) > 0 {
		read.Count = out.Counted[0].N
	}
	for _, s := range out.Summed {
		read.Minutes += int(s.Minutes)
	}
	if read.Backlog == nil {
		read.Backlog = []*dgraphStruct.DgraphTask{}
	}
	return read, nil
}
