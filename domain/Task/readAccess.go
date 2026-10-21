package Domain

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	"github.com/google/uuid"
)

// GetTasksReadAccessFor reads, for one person (by graph uid), what
// business/Task CanViewTask and business/WorkAccess decide each of the named
// tasks by: the person's place in its project, its assignee and its maker;
// with the task's name, its project's id, and its own and its project's
// deletion. One query for the lot; ids that aren't uuids are left out, and so
// is any task that doesn't exist.
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
					task_assignee { uid user_uuid }
					task_created_by { uid user_uuid }
					task_project {
						uid
						project_uuid
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

// GetTasksStandingFor reads, for one person by user uuid, what deciding
// whether they may see each of the named tasks takes beyond Postgres: its
// assignee and maker, and the person's place in its project. No task's
// content: this is the read the decision itself makes (business/WorkAccess
// restricted.go), for anyone, before anything is shown. Ids that aren't uuids
// are left out.
func GetTasksStandingFor(ctx context.Context, taskUUIDs []string, userUUID string) ([]*dgraphStruct.DgraphTask, error) {
	ids := make([]string, 0, len(taskUUIDs))
	for _, id := range taskUUIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			ids = append(ids, parsed.String())
		}
	}
	me, err := uuid.Parse(userUUID)
	if len(ids) == 0 || err != nil {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	query := `query Standing($user: string) {
				tasks(func: eq(task_uuid, ` + string(list) + `)) {
					task_uuid
					task_assignee { user_uuid }
					task_created_by { user_uuid }
					task_project {
						uid
						project_uuid
						project_deleted_at
						project_is_member: count(project_members @filter(eq(user_uuid, $user)))
						project_is_admin: count(project_admins @filter(eq(user_uuid, $user)))
					}
				}
			}`
	tasks, err := dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$user": me.String()})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTasksStandingFor err: %+v", err)
	}
	return tasks, err
}

// GetTasksBelow is the live tasks below a task, its sub-tasks and theirs, by
// uuid, as the reader of ctx may see them: what restricting it restricts with
// it (business/WorkAccess). Ten levels down at most, more than the app makes.
func GetTasksBelow(ctx context.Context, taskUUID string) ([]string, error) {
	id, err := uuid.Parse(taskUUID)
	if err != nil {
		return nil, nil
	}
	query := `query Below($task: string) {
				var(func: eq(task_uuid, $task)) @recurse(depth: 10, loop: false) { below as task_sub_tasks }
				tasks(func: uid(below)) @filter(` + dgraphquery.Live("task_deleted_at") + `) { task_uuid }
			}`
	tasks, err := dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$task": id.String()})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTasksBelow err: %+v", err)
		return nil, err
	}
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && t.Uuid != "" && !strings.EqualFold(t.Uuid, id.String()) {
			out = append(out, t.Uuid)
		}
	}
	return out, nil
}
