package business

// A schedule that acts for each task (package 9 R3): "every weekday at 17:00,
// for each open task that's overdue, tell its assignee".
//
// One run per scheduled time, whatever it acts on. It reads the project's
// open tasks once (at most taskDomain.MaxOpenTasks of them), as the rule's
// owner, who must still be in the project, and keeps those its conditions
// hold for. It then acts on at most maxEachTask of them, soonest due first,
// within eachTaskBudget, each step through the Task Writer as the owner like
// any rule's.
//
// The run records how many matched and keeps every step with its task, so
// the history says "Matched 240 · acted on 200" and Undo puts back each
// change. What it didn't reach waits for the next run, when the conditions
// are read again.

import (
	"context"
	"fmt"
	"time"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/models/taskread"
)

const (
	// maxEachTask bounds the tasks one run acts on.
	maxEachTask = 200
	// eachTaskBudget bounds one run's time, well inside the queue's minute
	// for a job.
	eachTaskBudget = 40 * time.Second
)

// runEachTask runs a schedule rule's steps for each open task of its project
// its conditions hold for, as one run.
func (cw *compiledWorkflow) runEachTask(ctx context.Context, ev triggerEvent, now time.Time) {
	if !access.allows(ctx, cw.createdBy, ev) {
		cw.recordSkip(ctx, ev, now, "Skipped: the rule's owner isn't in the project any more")
		return
	}
	owner, err := loadActor(ctx, cw.createdBy)
	if err != nil {
		cw.recordSkip(ctx, ev, now, "Skipped: the rule's owner can't act any more")
		return
	}
	// Read as nobody: what the run names and counts, every member reads in
	// its history, so a restricted task is never among them (models/taskread).
	byProject, err := openTasks(taskread.AsNobody(ctx), []string{cw.projectID}, nil, nil, taskDomain.MaxOpenTasks)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s each task: %v", cw.id, err)
		cw.recordSkip(ctx, ev, now, "Skipped: the project's tasks couldn't be read")
		return
	}
	var matched []*dgraphStruct.DgraphTask
	for _, t := range byProject[cw.projectID] {
		if cw.conditions.hold(factsOf(t), now, cw.loc) {
			matched = append(matched, t)
		}
	}
	n := len(matched)
	run := cw.newRun(ev, now)
	run.Matched, run.Branch = &n, "then"
	run.Subject = tasksWord(n)
	if reason, stop := limited(ctx, cw, now); reason != "" {
		if stop {
			cw.stop(ctx, reason)
		}
		cw.recordSkip(ctx, ev, now, reason)
		return
	}
	run.Status = workflowModel.RunRunning
	claimed, err := claimRun(ctx, run, ev.eventKey)
	if err != nil || !claimed {
		return // stored nothing, or this time already has its run
	}
	actionCtx := helpers.WithWorkflowGenerated(ctx)
	shared := &runShared{}
	deadline := clock().Add(eachTaskBudget)
	var firstErr string
	acted := 0
	for _, t := range matched {
		if acted == maxEachTask || clock().After(deadline) || ctx.Err() != nil {
			break
		}
		rc := &runCtx{owner: owner, task: t, runID: run.ID, shared: shared}
		tev := ev
		tev.taskID, tev.vars = t.Uuid, taskVars(t)
		for i, act := range cw.actions {
			if i >= MaxActionsPerBranch {
				break
			}
			step := cw.runTaskAction(actionCtx, act, tev, rc, i)
			if step.Status == "failed" && firstErr == "" {
				firstErr = step.Detail
			}
			run.Steps = append(run.Steps, step)
		}
		acted++
	}
	run.Status, run.Error = workflowModel.RunOK, firstErr
	if firstErr != "" {
		run.Status = workflowModel.RunFailed
	}
	if err := finishRun(ctx, run); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s finish run: %v", cw.id, err)
	}
	if err := recordRun(ctx, cw.id, now, firstErr); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s RecordRun err: %v", cw.id, err)
	}
}

// tasksWord is a count of tasks in words.
func tasksWord(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}
