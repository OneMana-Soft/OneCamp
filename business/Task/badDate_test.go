package business

import (
	"context"
	"errors"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A new task or sub-task whose due or start date isn't a date is refused
// (ErrBadDate, the asker's to correct) before anything is read or written: it
// failed only after the task's row was made, leaving a numbered row with no
// task behind it. No store is connected here, so a write that got past the
// check would fail the test.
func TestABadDateMakesNoTask(t *testing.T) {
	ctx := context.Background()
	who, project := &model.UserInfo{}, &dgraphStruct.DgraphProject{}
	for _, in := range []adapter.CreateOrUpdateTaskInput{
		{TaskName: "Venue", DueDate: "tomorrow"},
		{TaskName: "Venue", StartDate: "2026-10-23"},
	} {
		if _, err := CreateTask(ctx, uuid.New(), who, project, nil, in, nil); !errors.Is(err, ErrBadDate) {
			t.Errorf("a task due %q, starting %q: %v, want ErrBadDate", in.DueDate, in.StartDate, err)
		}
		if _, err := CreateSubTask(ctx, uuid.New(), who, project, nil, in); !errors.Is(err, ErrBadDate) {
			t.Errorf("a sub-task due %q, starting %q: %v, want ErrBadDate", in.DueDate, in.StartDate, err)
		}
	}
}
