package business

import (
	"context"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// A delegated task used to sit in Todo while its agent worked on it, so the
// board said nobody had started. Linear's guidance, and what people expect of a
// teammate: when work starts, the task says so.
func TestMarkStartedMovesOnlyUnstartedTasks(t *testing.T) {
	saved := moveDelegatedTask
	t.Cleanup(func() { moveDelegatedTask = saved })
	var moved []string
	moveDelegatedTask = func(_ context.Context, p *taskStatusPoster, status string) error {
		moved = append(moved, p.dgraphTask.Status+"->"+status)
		return nil
	}

	for _, from := range []string{dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_BACKLOG, dgraphStruct.TASK_STATUS_INPROGRESS, "inReview", "done"} {
		p := &taskStatusPoster{taskUUID: uuid.New(), dgraphTask: &dgraphStruct.DgraphTask{Status: from}}
		p.MarkStarted(context.Background())
		p.MarkStarted(context.Background()) // a second call never moves it again
	}
	want := []string{"todo->inProgress", "backlog->inProgress"}
	if len(moved) != len(want) || moved[0] != want[0] || moved[1] != want[1] {
		t.Fatalf("moved %v, want %v", moved, want)
	}

	var nilPoster *taskStatusPoster
	nilPoster.MarkStarted(context.Background()) // no status surface: nothing to do, no panic
}
