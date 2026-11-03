package business

import (
	"context"
	"errors"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Sub-tasks made together (CR-167 F1) read the project, with their task, and
// the maker's standing once, then are made in one batch under that task as
// it was read, never one at a time. A name with no words refuses before
// anything is read, and a maker who may only view makes none.
func TestSubTasksAreMadeTogetherUnderTheirTask(t *testing.T) {
	project, parent := uuid.New(), uuid.New()
	saved := struct {
		project  func(context.Context, string, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error)
		standing func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error)
		many     func(context.Context, uuid.UUID, *userModels.UserInfo, *dgraphStruct.DgraphProject, []taskBusiness.NewTask) ([]uuid.UUID, error)
		one      func(context.Context, uuid.UUID, *userModels.UserInfo, *dgraphStruct.DgraphProject, *dgraphStruct.DgraphUser, adapter.CreateOrUpdateTaskInput) (*dgraphStruct.DgraphTask, error)
	}{projectWithTask, standingIn, createTasks, createSubTask}
	t.Cleanup(func() {
		projectWithTask, standingIn, createTasks, createSubTask = saved.project, saved.standing, saved.many, saved.one
	})
	reads, standings, one, role := 0, 0, 0, workAccess.RoleMember
	projectWithTask = func(_ context.Context, p, task string, _ *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		reads++
		return &dgraphStruct.DgraphProject{Uid: "0x1", Uuid: p, Team: &dgraphStruct.DgraphTeam{Uid: "0x2"},
			Tasks: []*dgraphStruct.DgraphTask{{Uid: "0x7", Uuid: task, Name: "Checkout crashes"}}}, nil
	}
	standingIn = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		standings++
		return workAccess.Standing{Role: role}, nil
	}
	var batches [][]taskBusiness.NewTask
	createTasks = func(_ context.Context, _ uuid.UUID, _ *userModels.UserInfo, _ *dgraphStruct.DgraphProject, tasks []taskBusiness.NewTask) ([]uuid.UUID, error) {
		batches = append(batches, tasks)
		ids := make([]uuid.UUID, len(tasks))
		for i := range ids {
			ids[i] = uuid.New()
		}
		return ids, nil
	}
	createSubTask = func(context.Context, uuid.UUID, *userModels.UserInfo, *dgraphStruct.DgraphProject, *dgraphStruct.DgraphUser, adapter.CreateOrUpdateTaskInput) (*dgraphStruct.DgraphTask, error) {
		one++
		return &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}, nil
	}
	ctx := context.Background()
	under := &dgraphStruct.DgraphTask{Uuid: parent.String(), Project: &dgraphStruct.DgraphProject{Uuid: project.String()}}
	ids, err := MakeSubTasks(ctx, &userModels.UserInfo{}, under, []string{"Reproduce", "  Fix  the\ncrash ", "Verify"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || reads != 1 || standings != 1 || len(batches) != 1 || one != 0 {
		t.Fatalf("made %d with %d project reads, %d standings, %d batches and %d one at a time; want 3 made in one batch after one read of each",
			len(ids), reads, standings, len(batches), one)
	}
	for i, n := range batches[0] {
		want := []string{"Reproduce", "Fix the crash", "Verify"}[i]
		if n.Input.TaskName != want || n.Input.ProjectUuid != project.String() || n.Parent != -1 ||
			n.Under == nil || n.Under.Uid != "0x7" || n.Under.Uuid != parent.String() || n.Under.Name != "Checkout crashes" {
			t.Errorf("sub-task %d made as %+v under %+v, want %q under the task as read", i, n.Input, n.Under, want)
		}
	}
	reads, standings, batches, one = 0, 0, nil, 0
	if _, err := MakeSubTasks(ctx, &userModels.UserInfo{}, under, []string{"Fix", " \n "}); !errors.Is(err, ErrBadValue) || reads != 0 || len(batches) != 0 || one != 0 {
		t.Errorf("a name with no words: %v after %d reads, %d batches and %d one at a time; want ErrBadValue before anything", err, reads, len(batches), one)
	}
	role = workAccess.RoleViewer
	if _, err := MakeSubTasks(ctx, &userModels.UserInfo{}, under, []string{"Fix"}); err == nil || len(batches) != 0 || one != 0 {
		t.Errorf("a viewer made %d batches and %d one at a time: %v", len(batches), one, err)
	}
	if ids, err := MakeSubTasks(ctx, &userModels.UserInfo{}, under, nil); err != nil || len(ids) != 0 {
		t.Errorf("no names made %v: %v", ids, err)
	}
}
