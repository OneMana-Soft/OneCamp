package Domain

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
)

// GetDgraphTaskForTimer is what a task's SLA timer is matched and ended by
// (business/SLA): its status and since when, priority, label, when it was
// made and deleted, and its project. One read; the caller reads as the
// system (a timer serves the project, not a reader). nil for no such task.
func GetDgraphTaskForTimer(ctx context.Context, taskUUID string) (*dgraphStruct.DgraphTask, error) {
	t, err := dgraphModels.GetDgraphTaskInfoByUUID(ctx, `query TaskInfo($id: string){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid
					task_status
					task_status_since
					task_priority
					task_label
					task_created_at
					task_deleted_at
					task_project { project_uuid }
				}
			}`, map[string]string{"$id": taskUUID})
	if errors.Is(err, dgraphModels.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphTaskForTimer err: %+v", err)
	}
	return t, err
}

// GetDgraphTasksForAlerts is what an SLA alert tells of the tasks named, by
// uuid (business/SLA alerts.go): each one's name, status, deletion, project
// with its name, and assignee. One read, through the filtering model: the
// caller reads as the system (taskread.WithSystem) and tells each alert only
// to an assignee who may read the task.
func GetDgraphTasksForAlerts(ctx context.Context, taskUUIDs []string) (map[string]*dgraphStruct.DgraphTask, error) {
	out := map[string]*dgraphStruct.DgraphTask{}
	list := dgraphquery.UUIDList(taskUUIDs)
	if list == "" {
		return out, nil
	}
	tasks, err := dgraphModels.QueryDgraphTasks(ctx, `{
		q(func: eq(task_uuid, [`+list+`])) @filter(type(Task)) {
			uid
			task_uuid
			task_name
			task_status
			task_deleted_at
			task_project { uid project_uuid project_name }
			task_assignee { uid user_uuid }
		}
	}`, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphTasksForAlerts err: %+v", err)
		return nil, err
	}
	for _, t := range tasks {
		if t != nil && t.Uuid != "" {
			out[t.Uuid] = t
		}
	}
	return out, nil
}
