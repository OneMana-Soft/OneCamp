package business

// Estimates: how long a task should take, in minutes. The task's panel shows
// it beside the time logged on it, and the workload can count hours instead
// of tasks: each task's estimate spread over the working days it runs. In a
// project that estimates in points (business/EstimateScale), a task also has
// points, beside its hours.

import (
	"context"
	"errors"
	"strconv"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// MaxEstimateMinutes is the longest estimate a task can have: a thousand hours.
const MaxEstimateMinutes = 1000 * 60

var ErrEstimateRange = errors.New("an estimate is from 0 to 1000 hours")

// estimateOf is a task's estimate in minutes, 0 for none.
func estimateOf(t *dgraphStruct.DgraphTask) int {
	if t == nil || t.EstimateMinutes == nil || *t.EstimateMinutes < 0 {
		return 0
	}
	return *t.EstimateMinutes
}

// UpdateTaskEstimate sets how long a task should take; 0 takes the estimate
// off. A change goes into the task's history; the same estimate again writes
// nothing.
func UpdateTaskEstimate(ctx context.Context, taskUUID uuid.UUID, minutes int, task *dgraphStruct.DgraphTask, user *dgraphStruct.DgraphUser) error {
	if minutes < 0 || minutes > MaxEstimateMinutes {
		return ErrEstimateRange
	}
	if task.DeletedAt != nil && task.DeletedAt.Year() > 1970 {
		return ErrTaskDeleted
	}
	before := estimateOf(task)
	if before == minutes {
		return nil
	}
	now := time.Now()
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID.String(), UpdatedAt: &now, EstimateMinutes: &minutes,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid:      uuid.NewString(),
			Type:      dgraphStruct.ACTIVITY_TYPE_ESTIMATE,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: user.Uid},
			LogTime:   &now,
			PrevState: strconv.Itoa(before),
			NextState: strconv.Itoa(minutes),
		}}}
	if err := domain.UpdateTaskByTaskUUID(ctx, taskUUID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskEstimate postgres err: %+v", err)
		return err
	}
	if _, err := domain.CreateOrUpdateDgraphTask(ctx, write); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskEstimate dgraph err: %+v", err)
		return err
	}
	return nil
}

// UpdateTaskPoints sets how big a task is on its project's estimate scale, or
// takes its points off (nil). The points must be a size on the scale; a
// project that estimates in hours, or a task in none, takes none. A change
// goes into the task's history, named as the scale names it ("M"); the same
// points again write nothing.
func UpdateTaskPoints(ctx context.Context, taskUUID uuid.UUID, points *float64, task *dgraphStruct.DgraphTask, user *dgraphStruct.DgraphUser) error {
	if task.DeletedAt != nil && task.DeletedAt.Year() > 1970 {
		return ErrTaskDeleted
	}
	if task.Project == nil {
		return estimateScale.ErrHours
	}
	project, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		return err
	}
	scale, err := estimateScale.Of(ctx, project)
	if err != nil {
		return err
	}
	if scale == nil {
		return estimateScale.ErrHours
	}
	if points != nil && !estimateScale.Allows(scale, *points) {
		return estimateScale.ErrNotASize
	}
	before := task.EstimatePoints
	if (before == nil) == (points == nil) && (points == nil || *before == *points) {
		return nil
	}
	named := func(p *float64) string {
		if p == nil {
			return ""
		}
		return estimateScale.Label(scale, *p)
	}
	now := time.Now()
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID.String(), UpdatedAt: &now, EstimatePoints: points,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid:      uuid.NewString(),
			Type:      dgraphStruct.ACTIVITY_TYPE_POINTS,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: user.Uid},
			LogTime:   &now,
			PrevState: named(before),
			NextState: named(points),
		}}}
	if err := domain.UpdateTaskByTaskUUID(ctx, taskUUID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskPoints postgres err: %+v", err)
		return err
	}
	if _, err := domain.SetDgraphTaskPoints(ctx, write); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskPoints dgraph err: %+v", err)
		return err
	}
	return nil
}
