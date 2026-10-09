package business

// Recurring tasks, the way Asana does them: a task repeats on a schedule
// (every Monday, the 1st of each month) or a while after it was last done
// (two weeks after completion). Completing it creates the next occurrence,
// a copy with its dates moved on, and the rule moves to that task.
//
// It runs from UpdateTaskStatusByTaskUUID, so a task done by a person, an
// agent, a workflow or GitHub repeats the same way.

import (
	"context"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// RecurrenceError is a repeat the person asked for that can't be set; its
// text is written for them.
type RecurrenceError struct{ msg string }

func (e *RecurrenceError) Error() string { return e.msg }

// CheckRecurrence normalises a rule and mode from the client. Pure.
func CheckRecurrence(rule, mode string) (string, string, error) {
	norm, ok := schedulerBusiness.NormaliseCalendarRule(rule)
	if !ok {
		return "", "", &RecurrenceError{"Pick daily, weekly, monthly or yearly, every 1 to 365."}
	}
	switch mode {
	case "", recurrenceModel.ModeSchedule:
		mode = recurrenceModel.ModeSchedule
	case recurrenceModel.ModeCompletion:
		norm = schedulerBusiness.WithoutByDay(norm)
	default:
		return "", "", &RecurrenceError{"Repeat on a schedule or after completion."}
	}
	return norm, mode, nil
}

// SetTaskRecurrence makes a task repeat (rule "" stops it). Subtasks don't
// repeat: their copies would land under a parent that is already done.
// timeZone is the setter's (IANA): the days in the rule are their days.
func SetTaskRecurrence(ctx context.Context, task *dgraphStruct.DgraphTask, rule, mode, timeZone string, by uuid.UUID) (*recurrenceModel.TaskRecurrence, error) {
	taskUUID, err := uuid.Parse(task.Uuid)
	if err != nil {
		return nil, err
	}
	if rule == "" {
		return nil, recurrenceModel.Delete(taskUUID)
	}
	if task.ParentTask != nil && task.ParentTask.Uuid != "" {
		return nil, &RecurrenceError{"Subtasks can't repeat. Make the parent task repeat instead."}
	}
	rule, mode, err = CheckRecurrence(rule, mode)
	if err != nil {
		return nil, err
	}
	zone := ""
	if name := strings.TrimSpace(timeZone); name != "" {
		if _, err := time.LoadLocation(name); err != nil {
			return nil, &RecurrenceError{"That time zone isn't one we know."}
		}
		zone = name
	}
	anchor := 0
	if isSet(task.DueDate) {
		anchor = task.DueDate.In(helpers.Location(zone)).Day()
	}
	return recurrenceModel.Set(taskUUID, rule, mode, zone, anchor, by)
}

// GetTaskRecurrence is how the task repeats, or nil.
func GetTaskRecurrence(taskUUID uuid.UUID) (*recurrenceModel.TaskRecurrence, error) {
	return recurrenceModel.Get(taskUUID)
}

// isSet: tasks store "no date" as the zero time.
func isSet(t *time.Time) bool { return t != nil && t.Year() > 1970 }

// NextOccurrence is when the next one is due and starts, given the one just
// done, and the day of the month the series keeps from here. A task without a
// due date counts from when it was done; a start date keeps its distance
// before the due date.
//
// IN THE REPEAT'S TIME ZONE. Dates come back from the store in UTC, so a
// weekly repeat on Monday, due at midnight in India, was Sunday evening to the
// calendar and came out on Tuesday; "the day it was done" was the UTC day.
// Pure.
func NextOccurrence(r recurrenceModel.TaskRecurrence, due, start *time.Time, done time.Time) (nextDue time.Time, nextStart *time.Time, anchorDay int) {
	loc := helpers.Location(r.TimeZone)
	done = done.In(loc)
	base := done
	if isSet(due) {
		base = due.In(loc)
	}
	anchorDay = monthDay(r.AnchorDay, base)
	switch r.Mode {
	case recurrenceModel.ModeCompletion:
		// The day it was done, at the time of day it was due.
		b := time.Date(done.Year(), done.Month(), done.Day(), base.Hour(), base.Minute(), base.Second(), 0, loc)
		nextDue, _ = schedulerBusiness.NextRun(r.Rule, b, b)
	default:
		// The calendar's next date after the due date; a task done late
		// skips the dates already gone rather than arriving overdue.
		from := base
		if done.After(from) {
			from = done
		}
		nextDue, _ = schedulerBusiness.NextRunOnDay(r.Rule, from, base, anchorDay)
	}
	if isSet(start) && isSet(due) {
		s := nextDue.Add(start.Sub(*due))
		nextStart = &s
	}
	return nextDue, nextStart, anchorDay
}

// monthDay is the day of the month a monthly or yearly series lands on: the
// day it was set for, while the due date is still that day (or the month's
// last day standing in for it, as Feb 28 does for the 31st); once someone has
// moved the due date to another day, that day. 0 for a series with no day yet.
func monthDay(anchor int, due time.Time) int {
	last := time.Date(due.Year(), due.Month()+1, 0, 0, 0, 0, 0, due.Location()).Day()
	if anchor >= 1 && anchor <= 31 && min(anchor, last) == due.Day() {
		return anchor
	}
	return due.Day()
}

// repeatIfRecurring creates the next occurrence of a repeating task that was
// just done. Failures are logged, not returned: the status change itself
// happened, and the rule stays on the task to be retried on its next "done".
func repeatIfRecurring(ctx context.Context, taskUUID uuid.UUID, by *dgraphStruct.DgraphUser) {
	r, err := recurrenceModel.Take(taskUUID)
	if err != nil || r == nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/repeatIfRecurring take err: %+v", err)
		}
		return
	}
	putBack := func(why string, err error) {
		helpers.LogErrorWithContext(ctx, "business/repeatIfRecurring %s err: %+v", why, err)
		if perr := recurrenceModel.Put(r); perr != nil {
			helpers.LogErrorWithContext(ctx, "business/repeatIfRecurring put back err: %+v", perr)
		}
	}

	sys := helpers.WithSystemRead(ctx)
	task, err := GetDgraphTaskInfo(sys, taskUUID.String(), by.Uid)
	if err != nil || task == nil || task.Project == nil {
		putBack("read task", err)
		return
	}
	project, err := projectDomain.GetBasicDgraphProjectInfo(sys, task.Project.Uuid, by.Uid)
	if err != nil || project == nil || project.Team == nil {
		putBack("read project", err)
		return
	}
	var assignee *dgraphStruct.DgraphUser
	if task.Assignee != nil && task.Assignee.Uuid != "" {
		if assignee, err = userBusiness.GetDgraphUserInfoByUUID(ctx, task.Assignee.Uuid); err != nil {
			putBack("read assignee", err)
			return
		}
	}
	actorID, err := uuid.Parse(by.Uuid)
	if err != nil {
		putBack("actor id", err)
		return
	}

	due, start, anchorDay := NextOccurrence(*r, task.DueDate, task.StartDate, time.Now())
	in := adapter.CreateOrUpdateTaskInput{
		TaskName:    task.Name,
		ProjectUuid: project.Uuid,
		Priority:    task.Priority,
		Status:      dgraphStruct.TASK_STATUS_TODO,
		DueDate:     due.Format(time.RFC3339),
	}
	if task.Description != nil {
		in.TaskDescription = *task.Description
	}
	if task.Label != nil {
		in.Label = *task.Label
	}
	if start != nil {
		in.StartDate = start.Format(time.RFC3339)
	}
	if in.Priority == "" {
		in.Priority = dgraphStruct.TASK_PRIORITY_MEDIUM
	}
	actor := &model.UserInfo{UserPostgresInfo: model.User{Id: actorID}, UserDgraphInfo: *by}
	nextUUID, err := CreateTask(ctx, uuid.MustParse(project.Uuid), actor, project, assignee, in, nil)
	if err != nil {
		putBack("create next", err)
		return
	}
	// The next copy keeps the task's custom field values.
	if from, err := uuid.Parse(task.Uuid); err == nil {
		if err := taskFieldBusiness.CopyValues(ctx, from, nextUUID, actorID); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Task recurrence: copying field values err: %+v", err)
		}
	}
	r.TaskUUID = nextUUID
	r.AnchorDay = anchorDay
	if err := recurrenceModel.Put(r); err != nil {
		helpers.LogErrorWithContext(ctx, "business/repeatIfRecurring move rule err: %+v", err)
	}
}
