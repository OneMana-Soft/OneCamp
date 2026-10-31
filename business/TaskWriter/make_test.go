package business

import (
	"context"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A task made with files hands them to the task's making, which keeps only
// its maker's own uploads to the project (business/Task CreateTask's
// OwnFiles): a form's visitor files go on its task this way.
func TestATaskIsMadeWithItsFiles(t *testing.T) {
	project := uuid.New()
	saved := struct {
		project  func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error)
		standing func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error)
		create   func(context.Context, uuid.UUID, *userModels.UserInfo, *dgraphStruct.DgraphProject, *dgraphStruct.DgraphUser, adapter.CreateOrUpdateTaskInput, []*dgraphStruct.DgraphUser) (uuid.UUID, error)
	}{projectFor, standingIn, createTask}
	t.Cleanup(func() { projectFor, standingIn, createTask = saved.project, saved.standing, saved.create })
	projectFor = func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return &dgraphStruct.DgraphProject{Uid: "0x1", Uuid: project.String()}, nil
	}
	standingIn = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	var got []*dgraphStruct.DgraphAttachment
	createTask = func(_ context.Context, _ uuid.UUID, _ *userModels.UserInfo, _ *dgraphStruct.DgraphProject, _ *dgraphStruct.DgraphUser, in adapter.CreateOrUpdateTaskInput, _ []*dgraphStruct.DgraphUser) (uuid.UUID, error) {
		got = in.Attachments
		return uuid.New(), nil
	}
	files := []*dgraphStruct.DgraphAttachment{{Uuid: uuid.NewString(), FileName: "shot.png", ObjectKey: "forms/f/shot.png", Type: "image"}}
	if _, err := MakeTask(context.Background(), &userModels.UserInfo{}, project, NewTask{Name: "Checkout crashes", Attachments: files}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != files[0] {
		t.Errorf("the task was made with %v", got)
	}
}
