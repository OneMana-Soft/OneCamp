package Domain

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	"github.com/google/uuid"
)

// GetTasksReadAccessFor reads, for one person (by graph uid), what
// business/Task CanViewTask decides each of the named tasks by: the person's
// place in its project, its assignee and its maker; with the task's name and
// its own and its project's deletion. One query for the lot; ids that aren't
// uuids are left out, and so is any task that doesn't exist.
func GetTasksReadAccessFor(ctx context.Context, taskUUIDs []string, userUID string) ([]*dgraphStruct.DgraphTask, error) {
	ids := make([]string, 0, len(taskUUIDs))
	for _, id := range taskUUIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			ids = append(ids, parsed.String())
		}
	}
	if len(ids) == 0 || userUID == "" {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	query := `query Readable($user: string) {
				tasks(func: eq(task_uuid, ` + string(list) + `)) {
					task_uuid
					task_name
					task_deleted_at
					task_assignee { uid }
					task_created_by { uid }
					task_project {
						project_deleted_at
						project_is_member: count(project_members @filter(uid($user)))
						project_is_admin: count(project_admins @filter(uid($user)))
					}
				}
			}`
	tasks, err := dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$user": userUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTasksReadAccessFor err: %+v", err)
	}
	return tasks, err
}
