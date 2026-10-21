package business

// A thread and the task made from it (docs C5-2): "Make a task" ties them,
// and business/Task keeps resolved and done in step (threadTaskSync.go).

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// ErrTiedElsewhere is a thread tied to another task already.
var ErrTiedElsewhere = errors.New("threads: tied to another task")

// LinkTask ties a thread to the task made from it, for someone who may
// resolve the thread.
func LinkTask(ctx context.Context, who accessBusiness.Subject, threadID, taskUUID string) (Thread, error) {
	s, ok := Threads.(*service)
	if !ok {
		return Thread{}, ErrUnavailable
	}
	t, _, err := s.writable(ctx, who, threadID, actSettle)
	if err != nil {
		return Thread{}, err
	}
	got, err := s.store.setTask(ctx, t.ID, taskUUID, s.now())
	if err != nil {
		return Thread{}, err
	}
	if got == nil {
		return Thread{}, ErrTiedElsewhere
	}
	return *got, nil
}

// ThreadsOfTask is the threads tied to a task, read for the sync.
func ThreadsOfTask(ctx context.Context, taskUUID string) ([]Thread, error) {
	s, ok := Threads.(*service)
	if !ok {
		return nil, ErrUnavailable
	}
	return s.store.byTask(ctx, taskUUID)
}

// TiedThreads is up to limit threads tied to tasks, the most recently changed
// first, for the sync's reconciliation.
func TiedThreads(ctx context.Context, limit int) ([]Thread, error) {
	s, ok := Threads.(*service)
	if !ok {
		return nil, ErrUnavailable
	}
	return s.store.tied(ctx, limit)
}
