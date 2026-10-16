package business

import (
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphTaskModel "github.com/akashc777/OneCamp/models/dgraph/Task"
)

// A task named by id is read, then judged: one that isn't there, or that the
// person may not read, is not found; one they may read but not change is
// refused; and a read that failed stays a failure. Answered as "not found",
// a failed read told a project's admin their task was gone.
func TestTaskForJudgesTheRead(t *testing.T) {
	down := errors.New("graph unavailable")
	member := task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1"; x.Project.IsProjectMember = 1 })
	admin := task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1"; x.Project.IsProjectAdmin = 1 })
	for _, c := range []struct {
		name    string
		task    *dgraphStruct.DgraphTask
		readErr error
		change  bool
		want    error
	}{
		{"a task that isn't there", nil, dgraphTaskModel.ErrNotFound, false, ErrTaskNotVisible},
		{"a read that failed", nil, down, false, down},
		{"a read that failed, for a change", nil, down, true, down},
		{"a task the person may not read", task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1" }), nil, false, ErrTaskNotVisible},
		{"a member reading", member, nil, false, nil},
		{"a member changing", member, nil, true, ErrNotProjectAdmin},
		{"an admin changing", admin, nil, true, nil},
	} {
		got, err := taskAccess(c.task, c.readErr, viewer, c.change)
		if !errors.Is(err, c.want) || (c.want == nil) != (got != nil) {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
