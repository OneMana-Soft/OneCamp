package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
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
