package business

// A doc comment thread and the task made from it (docs C5-2): "Make a task"
// on a thread ties the two (business/Thread LinkTask), and from then on
// resolving either one resolves both.
//
//   - The task to the thread: a task done anywhere resolves its threads; done
//     no longer opens them again. It listens for task.status_changed, as the
//     doc to-dos do (taskDocSync.go), so no task write path knows of threads.
//     It resolves as whoever changed the task, through the thread's own rule:
//     someone who may not resolve the thread doesn't, by this either.
//   - The thread to the task: a thread resolved marks its task done, and one
//     opened again opens its task, as whoever did it, through the task's own
//     rule (Work Access), the one SetTodoDone uses.
//
// Neither side loops: a thread or task already in the state asked for changes
// nothing and says nothing. Events are delivered at most once
// (business/ResourceEvent), so a sweep settles what a lost event left apart:
// up to reconcileBudget tied threads each pass, the newer of the two changes
// winning.

import (
	"context"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	reconcileEvery = 5 * time.Minute
	// reconcileBudget is how many tied threads one sweep looks at, the most
	// recently changed first: a lost event is almost always a recent one.
	reconcileBudget = 200
)

// Seams for tests.
var (
	threadsOfTask = threadBusiness.ThreadsOfTask
	tiedThreads   = threadBusiness.TiedThreads
	settleThread  = func(ctx context.Context, who accessBusiness.Subject, threadID string, resolve bool) error {
		var err error
		if resolve {
			_, err = threadBusiness.Threads.Resolve(ctx, who, threadID)
		} else {
			_, err = threadBusiness.Threads.Reopen(ctx, who, threadID)
		}
		return err
	}
	personOf = func(ctx context.Context, userUUID string) (*model.UserInfo, error) {
		u, err := userBusiness.GetDgraphUserInfoByUUID(helpers.WithSystemRead(ctx), userUUID)
		if err != nil || u == nil {
			return nil, err
		}
		info := &model.UserInfo{}
		info.UserDgraphInfo = *u
		return info, nil
	}
	taskNow = func(ctx context.Context, taskUUID string) (*dgraphStruct.DgraphTask, error) {
		return GetDgraphTaskInfo(helpers.WithSystemRead(ctx), taskUUID, "")
	}
	setTaskDone = markTaskDone
	threadNow   = func(ctx context.Context, who accessBusiness.Subject, threadID string) (threadBusiness.Thread, error) {
		v, err := threadBusiness.Threads.Get(ctx, who, threadID)
		return v.Thread, err
	}
)

func init() {
	webhookBusiness.RegisterEventListener(taskToThreads)
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name: "thread.task",
		Wants: func(e resourceEventBusiness.Event) bool {
			return e.Kind == resourceEventBusiness.ThreadResolved || e.Kind == resourceEventBusiness.ThreadReopened
		},
		Run: threadToTask,
	})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "thread.task.reconcile", Every: reconcileEvery, Run: reconcileThreadTasks})
}

func subjectOf(u *model.UserInfo) accessBusiness.Subject {
	return accessBusiness.Subject{UserUID: u.UserDgraphInfo.Uid, UserUUID: u.UserDgraphInfo.Uuid}
}

// markTaskDone is a task done (or open again) as someone, by the task's rule.
func markTaskDone(ctx context.Context, user *model.UserInfo, taskUUID string, done bool) error {
	info, err := readTask(ctx, taskUUID, user.UserDgraphInfo.Uid)
	if err != nil {
		return err
	}
	if info == nil || !CanViewTask(info, user.UserDgraphInfo.Uid) {
		return ErrTaskNotVisible
	}
	if isDone(info.Status) == done {
		return nil
	}
	s, f, err := workAccess.OfTask(ctx, user.UserDgraphInfo.Uuid, user.UserDgraphInfo.Uid, info)
	if err != nil {
		return err
	}
	if err := s.CheckTask(workAccess.NeedEditTask, f); err != nil {
		return err
	}
	status := dgraphStruct.TASK_STATUS_TODO
	if done {
		status = dgraphStruct.TASK_STATUS_DONE
	}
	id, err := uuid.Parse(taskUUID)
	if err != nil {
		return err
	}
	return writeStatus(ctx, id, status, info, &user.UserDgraphInfo)
}

// taskToThreads: a task done resolves its threads; done no longer opens them.
func taskToThreads(ctx context.Context, eventType string, data map[string]interface{}) {
	if eventType != "task.status_changed" {
		return
	}
	move := taskStatusBusiness.MoveFromEvent(data)
	done := isDone(move.NewStatus)
	if done == isDone(move.OldStatus) {
		return
	}
	str := func(k string) string { s, _ := data[k].(string); return s }
	threads, err := threadsOfTask(ctx, str("task_id"))
	if err != nil || len(threads) == 0 {
		return
	}
	who, err := personOf(ctx, str("updated_by_uuid"))
	if err != nil || who == nil {
		return
	}
	for _, t := range threads {
		if (t.Status == threadBusiness.StatusResolved) == done {
			continue
		}
		if err := settleThread(ctx, subjectOf(who), t.ID, done); err != nil {
			helpers.LogErrorWithContext(ctx, "business/taskToThreads task %s thread %s: not settled as %s: %v", str("task_id"), t.ID, who.UserDgraphInfo.Uuid, err)
		}
	}
}

// threadToTask: a thread resolved marks its task done; opened again, open.
func threadToTask(ctx context.Context, e resourceEventBusiness.Event) {
	if e.Actor.UserUUID == "" || e.Actor.AgentUUID != "" {
		return // an agent settling a thread leaves the task to its people
	}
	threadID := e.Data["thread_id"]
	who, err := personOf(ctx, e.Actor.UserUUID)
	if err != nil || who == nil {
		return
	}
	t, err := threadNow(ctx, subjectOf(who), threadID)
	if err != nil || t.TaskUUID == "" {
		return
	}
	done := e.Kind == resourceEventBusiness.ThreadResolved
	if err := setTaskDone(ctx, who, t.TaskUUID, done); err != nil {
		helpers.LogErrorWithContext(ctx, "business/threadToTask thread %s task %s: not marked as %s: %v", threadID, t.TaskUUID, who.UserDgraphInfo.Uuid, err)
	}
}

// reconcileThreadTasks settles tied pairs a lost event left apart: the
// newer change wins, made as whoever made it.
func reconcileThreadTasks(ctx context.Context) error {
	threads, err := tiedThreads(ctx, reconcileBudget)
	if err != nil {
		return err
	}
	for _, t := range threads {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task, err := taskNow(ctx, t.TaskUUID)
		if err != nil || task == nil {
			continue
		}
		resolved, done := t.Status == threadBusiness.StatusResolved, isDone(task.Status)
		if resolved == done {
			continue
		}
		taskChanged := time.Time{}
		if task.StatusSince != nil {
			taskChanged = *task.StatusSince
		}
		if taskChanged.After(t.UpdatedAt) {
			// The task moved last: the thread follows, as the person who
			// opened it (a task doesn't keep who moved it; its event did).
			by := t.CreatedBy
			if who, err := personOf(ctx, by); err == nil && who != nil {
				_ = settleThread(ctx, subjectOf(who), t.ID, done)
			}
			continue
		}
		// The thread moved last: the task follows, as whoever settled it.
		by := t.ResolvedBy
		if by == "" {
			by = t.CreatedBy
		}
		if who, err := personOf(ctx, by); err == nil && who != nil {
			_ = setTaskDone(ctx, who, t.TaskUUID, resolved)
		}
	}
	return nil
}
