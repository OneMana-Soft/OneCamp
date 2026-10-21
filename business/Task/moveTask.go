package business

import (
	"context"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// MoveTask puts a task where it was dropped on a board: into status (a
// built-in key or one of the project's custom statuses), between
// the cards named beforeUUID (above it) and afterUUID (below it); either may be
// empty at the ends of a column. A change of status goes through
// UpdateTaskStatusByTaskUUID, so activity, search, GitHub sync and webhooks see
// it as they always have. The position is kept in the project's one order (Reorder).
func MoveTask(ctx context.Context, taskUUID uuid.UUID, status string, beforeUUID, afterUUID string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) error {
	projectUUID := ""
	if dgraphTaskInfo.Project != nil {
		projectUUID = dgraphTaskInfo.Project.Uuid
	}
	// An unknown column is refused before anything moves.
	if _, err := taskStatusBusiness.Resolve(ctx, projectUUID, status); err != nil {
		return err
	}
	// Does nothing when the card stayed in its column.
	if err := UpdateTaskStatusByTaskUUID(ctx, taskUUID, status, dgraphTaskInfo, userInfo); err != nil {
		return err
	}
	// Its place is in the project's one order, which every view shares.
	if err := Reorder(ctx, projectUUID, []*dgraphStruct.DgraphTask{{Uuid: taskUUID.String()}}, beforeUUID, afterUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/MoveTask failed to place the task err: %+v", err)
		return err
	}
	return nil
}
