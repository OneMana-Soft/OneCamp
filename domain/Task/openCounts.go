package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// maxCountedTasks bounds the open tasks one count reads.
const maxCountedTasks = 5000

// OpenTaskCountsByAssignee is how many open tasks (not done, not cancelled,
// not deleted) each person has in a project, by their id. A person with none
// isn't in it. It reads at most the 5,000 newest open tasks. A restricted task
// the reader of ctx may not see isn't counted (models/taskread).
func OpenTaskCountsByAssignee(ctx context.Context, projectUUID string) (map[string]int, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	query := tr.Fill(`query Open($project: string){
		project(func: eq(project_uuid, $project)) {
			tasks: project_tasks(first: 5000, orderdesc: task_created_at) @filter(` + dgraphquery.Live("task_deleted_at") + ` AND NOT eq(task_status, "done") AND NOT eq(task_status, "canceled") AND has(task_assignee) ` + taskread.Mark + `) {
				task_assignee { user_uuid }
			}
		}
	}`)
	var out struct {
		Project []struct {
			Tasks []struct {
				Assignee *struct {
					UUID string `json:"user_uuid"`
				} `json:"task_assignee"`
			} `json:"tasks"`
		} `json:"project"`
	}
	if err := dgraphInit.ReadInto(ctx, query, map[string]string{"$project": projectUUID}, &out); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, p := range out.Project {
		for i, t := range p.Tasks {
			if i >= maxCountedTasks {
				break
			}
			if t.Assignee != nil && t.Assignee.UUID != "" {
				counts[t.Assignee.UUID]++
			}
		}
	}
	return counts, nil
}

// TaskNames is the names of live tasks by id (a deleted or unknown one isn't
// in it, nor a restricted one the reader of ctx may not see), at most 200 at
// a time.
func TaskNames(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	query := tr.Fill(fmt.Sprintf(`{
		tasks(func: eq(task_uuid, %s)) @filter(%s %s) {
			task_uuid
			task_name
		}
	}`, b, dgraphquery.Live("task_deleted_at"), taskread.Mark))
	var out struct {
		Tasks []struct {
			UUID string `json:"task_uuid"`
			Name string `json:"task_name"`
		} `json:"tasks"`
	}
	if err := dgraphInit.ReadInto(ctx, query, nil, &out); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(out.Tasks))
	for _, t := range out.Tasks {
		names[strings.ToLower(t.UUID)] = t.Name
	}
	return names, nil
}

// MaxOpenTasks bounds the open tasks one OpenTasks read answers for a project.
const MaxOpenTasks = 2000

// OpenTasks is these projects' open tasks (not done, not cancelled, not
// deleted), by project, in one read: in each, soonest due first and those
// with no due date last, at most perProject (and at most MaxOpenTasks). With
// a due window, only the tasks due in it, both ends counted in. A restricted
// task the reader of ctx may not see isn't in it (models/taskread): a rule's
// time triggers read as nobody, so what they name and count is what every
// member may see. Each task carries what a rule reads of it: its name,
// status, priority, tags, due date, assignee, creator and project.
func OpenTasks(ctx context.Context, projectUUIDs []string, dueFrom, dueTo *time.Time, perProject int) (map[string][]*dgraphStruct.DgraphTask, error) {
	out := make(map[string][]*dgraphStruct.DgraphTask, len(projectUUIDs))
	if len(projectUUIDs) == 0 {
		return out, nil
	}
	if perProject <= 0 || perProject > MaxOpenTasks {
		perProject = MaxOpenTasks
	}
	ids, err := json.Marshal(projectUUIDs)
	if err != nil {
		return nil, err
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	filter := dgraphquery.Live("task_deleted_at") + ` AND ` + dgraphStruct.TASK_OPEN_FILTER
	if dueFrom != nil {
		filter += fmt.Sprintf(` AND ge(task_due_date, "%s")`, dueFrom.UTC().Format(time.RFC3339))
	}
	if dueTo != nil {
		filter += fmt.Sprintf(` AND le(task_due_date, "%s")`, dueTo.UTC().Format(time.RFC3339))
	}
	query := tr.Fill(fmt.Sprintf(`{
		project(func: eq(project_uuid, %s)) {
			project_uuid
			tasks: project_tasks(first: %d, orderasc: task_due_date) @filter(%s %s) {
				task_uuid
				task_name
				task_status
				task_custom_status
				task_custom_status_name
				task_priority
				task_label
				task_due_date
				task_assignee { user_uuid }
				task_created_by { user_uuid }
			}
		}
	}`, ids, perProject, filter, taskread.Mark))
	var res struct {
		Project []struct {
			UUID  string                     `json:"project_uuid"`
			Tasks []*dgraphStruct.DgraphTask `json:"tasks"`
		} `json:"project"`
	}
	if err := dgraphInit.ReadInto(ctx, query, nil, &res); err != nil {
		return nil, err
	}
	for _, p := range res.Project {
		for _, t := range p.Tasks {
			if t == nil || t.Uuid == "" || len(out[p.UUID]) == perProject {
				continue
			}
			t.Project = &dgraphStruct.DgraphProject{Uuid: p.UUID}
			out[p.UUID] = append(out[p.UUID], t)
		}
	}
	return out, nil
}
