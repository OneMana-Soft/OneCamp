package business

import (
	"context"

	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// MoveTask puts a task where it was dropped on a board: into status (a
// built-in key or one of the project's custom statuses), between
// the cards named beforeUUID (above it) and afterUUID (below it); either may be
// empty at the ends of a column. A change of status goes through
// UpdateTaskStatusByTaskUUID, so activity, search, GitHub sync and webhooks see
// it as they always have. The position is kept; see business/TaskRank.
func MoveTask(ctx context.Context, taskUUID uuid.UUID, status string, beforeUUID, afterUUID string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) error {
	projectUUID := ""
	if dgraphTaskInfo.Project != nil {
		projectUUID = dgraphTaskInfo.Project.Uuid
	}
	target, err := taskStatusBusiness.Resolve(ctx, projectUUID, status)
	if err != nil {
		return err
	}
	// Does nothing when the card stayed in its column.
	if err := UpdateTaskStatusByTaskUUID(ctx, taskUUID, status, dgraphTaskInfo, userInfo); err != nil {
		return err
	}

	var named []string
	for _, id := range []string{beforeUUID, afterUUID} {
		if id != "" && id != taskUUID.String() {
			named = append(named, id)
		}
	}
	neighbours, err := domain.GetDgraphTaskRanks(ctx, named)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/MoveTask failed to read neighbour ranks err: %+v", err)
		return err
	}
	var before, after *dgraphStruct.DgraphTask
	for _, t := range neighbours {
		switch t.Uuid {
		case beforeUUID:
			before = t
		case afterUUID:
			after = t
		}
	}

	moved := &dgraphStruct.DgraphTask{Uuid: taskUUID.String()}
	if rank, ok := taskrank.Between(before, after); ok {
		moved.Rank = &rank
		return domain.SetDgraphTaskRanks(ctx, []*dgraphStruct.DgraphTask{moved})
	}

	// No room between the neighbours: renumber the column in this project.
	// Ranks are compared within a category, custom columns included, so their
	// order among each other survives the renumbering.
	column, err := domain.GetDgraphProjectColumnRanks(ctx, projectUUID, target.Category)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/MoveTask failed to read the column err: %+v", err)
		return err
	}
	return domain.SetDgraphTaskRanks(ctx, taskrank.Renumber(column, moved, before, after))
}
