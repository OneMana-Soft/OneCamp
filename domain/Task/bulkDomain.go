package Domain

// Bulk writes' reads and writes (PR2): many tasks in one query, one
// statement or one mutation.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	models "github.com/akashc777/OneCamp/models/postgres/Task"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// GetDgraphTasksForWrite reads tasks to change, by uuid, with what every
// task writer and Work Access read of one (the single-task read's fields), in
// one query; a task the reader (by graph uid) may not see isn't answered.
func GetDgraphTasksForWrite(ctx context.Context, ids []string, userDgraphUID string) ([]*dgraphStruct.DgraphTask, error) {
	var clean []string
	for _, id := range ids {
		if p, err := uuid.Parse(id); err == nil {
			clean = append(clean, p.String())
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(userDgraphUID) == "" {
		userDgraphUID = "0x1"
	}
	list, _ := json.Marshal(clean)
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	query := tr.Fill(`query Tasks($userUid: string){
		tasks(func: eq(task_uuid, ` + string(list) + `)) @filter(has(task_uuid) @@hidden_tasks@@) {
			uid
			task_uuid
			task_name
			task_status
			task_custom_status
			task_custom_status_name
			task_priority
			task_label
			task_start_date
			task_due_date
			task_deleted_at
			task_estimate_minutes
			task_team { team_name team_uuid }
			task_project {
				uid
				project_uuid
				project_name
				project_is_member: count(project_members @filter(uid($userUid)))
				project_is_admin: count(project_admins @filter(uid($userUid)))
			}
			task_attachments { attachment_uuid attachment_obj_key attachment_file_name }
			task_assignee { uid user_uuid user_name user_profile_object_key }
			task_parent_task { uid task_uuid task_name }
			task_created_by { uid user_uuid }
		}
	}`)
	return dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$userUid": userDgraphUID})
}

// TouchTasks marks tasks changed at at, in one statement.
func TouchTasks(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if err := models.TouchTasks(ctx, ids, at); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TouchTasks couldn't mark %d tasks changed err: %+v", len(ids), err)
		return err
	}
	return nil
}

// SetDgraphTasks writes tasks already in the graph (each by its uid), in one
// mutation.
func SetDgraphTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	if err := dgraphModels.SetDgraphTasks(ctx, tasks); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetDgraphTasks couldn't write %d tasks err: %+v", len(tasks), err)
		return err
	}
	return nil
}

// GitHubIssueLink is a task and the issue it links.
type GitHubIssueLink = models.GitHubIssueLink

// SetGitHubIssueFieldsOnTasks links tasks to their issues, in one statement.
func SetGitHubIssueFieldsOnTasks(ctx context.Context, links []GitHubIssueLink) error {
	if err := models.SetGitHubIssueFieldsOnTasks(ctx, links); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubIssueFieldsOnTasks err: %+v", err)
		return err
	}
	return nil
}

// ClearGitHubLinkFromTasks unlinks tasks from GitHub, in one statement.
func ClearGitHubLinkFromTasks(ctx context.Context, ids []uuid.UUID) error {
	if err := models.ClearGitHubLinkFromTasks(ctx, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ClearGitHubLinkFromTasks err: %+v", err)
		return err
	}
	return nil
}

// CreateTasks makes many tasks' rows in a project, numbered in the order
// given, in one statement (the BE audit's #30); a task the project has no key
// for yet gets 0.
func CreateTasks(ctx context.Context, ids []uuid.UUID, projectUUID, createdBy uuid.UUID, createdAt time.Time) (map[uuid.UUID]int, error) {
	numbers, err := taskKeyModel.InsertTasks(ctx, ids, projectUUID, createdBy, createdAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateTasks Failed to create %d tasks err: %+v", len(ids), err)
	}
	return numbers, err
}

// CreateDgraphTasks writes new tasks and the nodes they hang from in one mutation.
func CreateDgraphTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask, others ...any) (map[string]string, error) {
	for _, t := range tasks {
		t.DType = []string{"Task"}
	}
	return dgraphModels.CreateDgraphTasks(ctx, tasks, others...)
}

// CreateTasksInOpenSearch indexes new tasks in one _bulk request.
func CreateTasksInOpenSearch(ctx context.Context, tasks []*openSearchStruct.OpenSearchTask) {
	var b strings.Builder
	for _, t := range tasks {
		doc, err := json.Marshal(t)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/CreateTasksInOpenSearch couldn't write task %s err: %+v", t.Uuid, err)
			continue
		}
		fmt.Fprintf(&b, "{ \"create\" : { \"_index\" : %q, \"_id\" : %q } }\n%s\n", openSearchStruct.TASK_INDEX, t.Uuid, doc)
	}
	if b.Len() == 0 {
		return
	}
	if err := OpenSearchBulkModels.BulkCreateInOpenSearch(b.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateTasksInOpenSearch couldn't index %d tasks err: %+v", len(tasks), err)
	}
}
