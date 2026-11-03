package business

import (
	"context"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A task made with a start and a type starts then and has that type, given
// as its maker, the way a template's task is made; one made with neither
// leaves both alone.
func TestATaskIsMadeWithItsStartAndType(t *testing.T) {
	project, made := uuid.New(), uuid.New()
	saved := struct {
		project  func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error)
		standing func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error)
		create   func(context.Context, uuid.UUID, *userModels.UserInfo, *dgraphStruct.DgraphProject, *dgraphStruct.DgraphUser, adapter.CreateOrUpdateTaskInput, []*dgraphStruct.DgraphUser) (uuid.UUID, error)
		setType  func(context.Context, *dgraphStruct.DgraphTask, *uuid.UUID, *dgraphStruct.DgraphUser) (*taskKeyModel.Type, error)
	}{projectFor, standingIn, createTask, setTaskType}
	t.Cleanup(func() {
		projectFor, standingIn, createTask, setTaskType = saved.project, saved.standing, saved.create, saved.setType
	})
	projectFor = func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return &dgraphStruct.DgraphProject{Uid: "0x1", Uuid: project.String()}, nil
	}
	standingIn = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	var start string
	createTask = func(_ context.Context, _ uuid.UUID, _ *userModels.UserInfo, _ *dgraphStruct.DgraphProject, _ *dgraphStruct.DgraphUser, in adapter.CreateOrUpdateTaskInput, _ []*dgraphStruct.DgraphUser) (uuid.UUID, error) {
		start = in.StartDate
		return made, nil
	}
	var typed []string
	setTaskType = func(_ context.Context, task *dgraphStruct.DgraphTask, typeID *uuid.UUID, by *dgraphStruct.DgraphUser) (*taskKeyModel.Type, error) {
		typed = append(typed, task.Uuid+" "+typeID.String()+" "+by.Uid)
		return &taskKeyModel.Type{ID: *typeID}, nil
	}
	maker := &userModels.UserInfo{}
	maker.UserDgraphInfo.Uid = "0x9"
	on := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	bug := uuid.New()
	if _, err := MakeTask(context.Background(), maker, project, NewTask{Name: "Checkout crashes", Start: &on, Type: &bug}); err != nil {
		t.Fatal(err)
	}
	if start != on.Format(time.RFC3339) {
		t.Errorf("the task starts %q, want %q", start, on.Format(time.RFC3339))
	}
	if want := made.String() + " " + bug.String() + " 0x9"; len(typed) != 1 || typed[0] != want {
		t.Errorf("the type was set %v, want once as %q", typed, want)
	}
	start, typed = "x", nil
	if _, err := MakeTask(context.Background(), maker, project, NewTask{Name: "Plain"}); err != nil {
		t.Fatal(err)
	}
	if start != "" || len(typed) != 0 {
		t.Errorf("a task with no start or type got start %q and types %v", start, typed)
	}
	// Task, chosen (a template's required type met by it, CR-167 F2), is
	// what a task is with no type: nothing is set.
	task := workTaxonomy.TypeTask
	if _, err := MakeTask(context.Background(), maker, project, NewTask{Name: "Chosen as a Task", Type: &task}); err != nil {
		t.Fatal(err)
	}
	if len(typed) != 0 {
		t.Errorf("a task made as a Task had its type set: %v", typed)
	}
}
