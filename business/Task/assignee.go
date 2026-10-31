package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// assigneeWrite is the one write of a task's assignee: to (nil: nobody) in
// place of from, by by at at, as the graph has it (the edge, and an activity
// from whom to whom) and as search has it, with the task's deletion time
// written as deletedAt: the zero time for a live task, so a late write never
// leaves one deleted (UpdateTaskAssigneeByTaskUUID refuses a deleted one), or
// the time it went to the trash, where it stays (AssignInTrash). Pure.
func assigneeWrite(taskUUID string, from, to, by *dgraphStruct.DgraphUser, at, deletedAt time.Time) (*dgraphStruct.DgraphTask, *openSearchStruct.OpenSearchTask) {
	activity := &dgraphStruct.DgraphTaskActivity{Uuid: uuid.NewString(), Type: dgraphStruct.ACTIVITY_TYPE_ASSIGNEE,
		CreatedBy: &dgraphStruct.DgraphUser{Uid: by.Uid}, LogTime: &at}
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID, UpdatedAt: &at, DeletedAt: &deletedAt,
		Activity: []*dgraphStruct.DgraphTaskActivity{activity}}
	none := ""
	search := &openSearchStruct.OpenSearchTask{Uuid: taskUUID, TaskUpdatedAt: at.Unix(), TaskAssigneeUuid: &none}
	if !deletedAt.IsZero() {
		trashedAt := deletedAt.Unix()
		search.TaskDeletedAt = &trashedAt
	}
	if from != nil {
		activity.PrevState = from.DisplayName()
	}
	if to != nil {
		write.Assignee = &dgraphStruct.DgraphUser{Uid: to.Uid, Tasks: []*dgraphStruct.DgraphTask{{Uid: "uid(task)"}}}
		activity.NextState = to.DisplayName()
		search.TaskAssigneeUuid, search.TaskAssigneeFullName = &to.Uuid, to.DisplayName()
	}
	return write, search
}

// AssignInTrash hands a task in the trash to to (nil takes its assignee off)
// and leaves it there, by by: the graph's edge and the search entry's
// assignee, with its deletion time as it was, so a restore brings it back as
// left. An old import's re-check takes off an assignee its source doesn't let
// in, and an admin's undo of it puts them back, whatever the task's state
// (business/Import recheck.go). Handed only to someone who can be given work,
// as every path that assigns.
func AssignInTrash(ctx context.Context, task *dgraphStruct.DgraphTask, to, by *dgraphStruct.DgraphUser) error {
	if to != nil {
		if err := principalBusiness.Unassignable(to, principalBusiness.Assigned); err != nil {
			return err
		}
	}
	from := ""
	if task.Assignee != nil {
		from = task.Assignee.Uid
	}
	write, search := assigneeWrite(task.Uuid, task.Assignee, to, by, time.Now(), *task.DeletedAt)
	if _, err := domain.UpdateDgraphTaskAssignee(ctx, write, from, task.Uid); err != nil {
		return err
	}
	domain.UpdateTaskInOpenSearch(search)
	return nil
}
