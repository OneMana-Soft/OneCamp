package business

import (
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphTaskModel "github.com/akashc777/OneCamp/models/dgraph/Task"
)

// A task named by id is read, then judged readable or not: one that isn't
// there, or that the person may not read, is not found, and a read that
// failed stays a failure. Answered as "not found", a failed read told a
// project's admin their task was gone. What they may do with a task they may
// read is Work Access's rule (business/WorkAccess rule_test.go).
func TestTaskForJudgesTheRead(t *testing.T) {
	down := errors.New("graph unavailable")
	member := task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1"; x.Project.IsProjectMember = 1 })
	admin := task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1"; x.Project.IsProjectAdmin = 1 })
	for _, c := range []struct {
		name    string
		task    *dgraphStruct.DgraphTask
		readErr error
		want    error
	}{
		{"a task that isn't there", nil, dgraphTaskModel.ErrNotFound, ErrTaskNotVisible},
		{"a read that failed", nil, down, down},
		{"a task the person may not read", task(func(x *dgraphStruct.DgraphTask) { x.Uuid = "t1" }), nil, ErrTaskNotVisible},
		{"a member reading", member, nil, nil},
		{"an admin reading", admin, nil, nil},
	} {
		got, err := readable(c.task, c.readErr)(viewer)
		if !errors.Is(err, c.want) || (c.want == nil) != (got != nil) {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
