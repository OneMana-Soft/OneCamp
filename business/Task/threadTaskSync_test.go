package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// syncWorld is one thread tied to one task, both in memory.
type syncWorld struct {
	thread   threadBusiness.Thread
	taskDone bool
	taskAt   time.Time
	settled  int // thread changes made
	marked   int // task changes made
}

func newSyncWorld(t *testing.T) *syncWorld {
	w := &syncWorld{thread: threadBusiness.Thread{ID: "th1", TaskUUID: "8a1b2c3d-4e5f-4a6b-9c7d-8e9f0a1b2c3d", Status: threadBusiness.StatusOpen, CreatedBy: "u-ana"}}
	saved := [...]any{threadsOfTask, tiedThreads, settleThread, personOf, taskNow, setTaskDone, threadNow}
	threadsOfTask = func(context.Context, string) ([]threadBusiness.Thread, error) {
		return []threadBusiness.Thread{w.thread}, nil
	}
	tiedThreads = func(context.Context, int) ([]threadBusiness.Thread, error) {
		return []threadBusiness.Thread{w.thread}, nil
	}
	settleThread = func(_ context.Context, _ accessBusiness.Subject, _ string, resolve bool) error {
		want := threadBusiness.StatusOpen
		if resolve {
			want = threadBusiness.StatusResolved
		}
		if w.thread.Status != want {
			w.thread.Status, w.thread.UpdatedAt = want, time.Now()
			w.settled++
		}
		return nil
	}
	personOf = func(_ context.Context, id string) (*model.UserInfo, error) {
		u := &model.UserInfo{}
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: id, Uid: "0x" + id}
		return u, nil
	}
	taskNow = func(context.Context, string) (*dgraphStruct.DgraphTask, error) {
		status := dgraphStruct.TASK_STATUS_TODO
		if w.taskDone {
			status = dgraphStruct.TASK_STATUS_DONE
		}
		at := w.taskAt
		return &dgraphStruct.DgraphTask{Status: status, StatusSince: &at}, nil
	}
	setTaskDone = func(_ context.Context, _ *model.UserInfo, _ string, done bool) error {
		if w.taskDone != done {
			w.taskDone, w.taskAt = done, time.Now()
			w.marked++
		}
		return nil
	}
	threadNow = func(context.Context, accessBusiness.Subject, string) (threadBusiness.Thread, error) {
		return w.thread, nil
	}
	t.Cleanup(func() {
		threadsOfTask = saved[0].(func(context.Context, string) ([]threadBusiness.Thread, error))
		tiedThreads = saved[1].(func(context.Context, int) ([]threadBusiness.Thread, error))
		settleThread = saved[2].(func(context.Context, accessBusiness.Subject, string, bool) error)
		personOf = saved[3].(func(context.Context, string) (*model.UserInfo, error))
		taskNow = saved[4].(func(context.Context, string) (*dgraphStruct.DgraphTask, error))
		setTaskDone = saved[5].(func(context.Context, *model.UserInfo, string, bool) error)
		threadNow = saved[6].(func(context.Context, accessBusiness.Subject, string) (threadBusiness.Thread, error))
	})
	return w
}

func statusEvent(old, new string) map[string]interface{} {
	return map[string]interface{}{"task_id": "8a1b2c3d-4e5f-4a6b-9c7d-8e9f0a1b2c3d", "updated_by_uuid": "u-ben", "old_status": old, "new_status": new}
}

func TestATaskDoneResolvesItsThreadAndOpenAgainReopensIt(t *testing.T) {
	w := newSyncWorld(t)
	taskToThreads(context.Background(), "task.status_changed", statusEvent(dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_DONE))
	if w.thread.Status != threadBusiness.StatusResolved {
		t.Fatalf("the thread is %s", w.thread.Status)
	}
	taskToThreads(context.Background(), "task.status_changed", statusEvent(dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_TODO))
	if w.thread.Status != threadBusiness.StatusOpen {
		t.Fatalf("reopened, the thread is %s", w.thread.Status)
	}
	// A move that leaves it done (or not done) changes nothing.
	before := w.settled
	taskToThreads(context.Background(), "task.status_changed", statusEvent(dgraphStruct.TASK_STATUS_TODO, "in_progress"))
	if w.settled != before {
		t.Error("a move between not-done states settled the thread")
	}
}

func TestAThreadResolvedMarksItsTaskDoneButNeverForAnAgent(t *testing.T) {
	w := newSyncWorld(t)
	resolved := resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadResolved, Actor: resourceEventBusiness.Actor{UserUUID: "u-ana"}, Data: map[string]string{"thread_id": "th1"}}
	threadToTask(context.Background(), resolved)
	if !w.taskDone {
		t.Fatal("the task isn't done")
	}
	threadToTask(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadReopened, Actor: resourceEventBusiness.Actor{UserUUID: "u-ana"}, Data: map[string]string{"thread_id": "th1"}})
	if w.taskDone {
		t.Fatal("reopened, the task is still done")
	}
	agent := resolved
	agent.Actor = resourceEventBusiness.Actor{UserUUID: "u-ana", AgentUUID: "captain"}
	threadToTask(context.Background(), agent)
	if w.taskDone {
		t.Error("an agent's resolve marked the task done")
	}
}

// Neither side loops: each change made once, however the events bounce.
func TestTheSyncNeverLoops(t *testing.T) {
	w := newSyncWorld(t)
	taskToThreads(context.Background(), "task.status_changed", statusEvent(dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_DONE))
	w.taskDone = true
	// The thread's resolve comes back as its event: the task is done already.
	threadToTask(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadResolved, Actor: resourceEventBusiness.Actor{UserUUID: "u-ben"}, Data: map[string]string{"thread_id": "th1"}})
	if w.settled != 1 || w.marked != 0 {
		t.Errorf("thread changed %d times, task %d", w.settled, w.marked)
	}
}

// Events are delivered at most once: a lost one is settled by the sweep, the
// newer change winning.
func TestAPairALostEventLeftApartConvergesAtTheNextSweep(t *testing.T) {
	w := newSyncWorld(t)
	// The thread was resolved, and its event lost: the task never heard.
	w.taskAt = time.Now().Add(-time.Hour)
	w.thread.Status, w.thread.UpdatedAt, w.thread.ResolvedBy = threadBusiness.StatusResolved, time.Now(), "u-ana"
	if err := reconcileThreadTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.taskDone {
		t.Fatal("the task didn't follow the thread")
	}
	// The task was opened again later, and that event lost too.
	w.taskDone, w.taskAt = false, time.Now().Add(time.Minute)
	if err := reconcileThreadTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.thread.Status != threadBusiness.StatusOpen {
		t.Fatal("the thread didn't follow the task")
	}
	// Agreeing, a sweep changes nothing.
	settled, marked := w.settled, w.marked
	if err := reconcileThreadTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.settled != settled || w.marked != marked {
		t.Error("a sweep changed a pair that agreed")
	}
}
