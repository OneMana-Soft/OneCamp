package business

import (
	"context"
	"errors"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A bulk create's first statuses are judged with one read of the project's
// rules for the whole batch, and the maker's standing read once, for the
// admins' rule: each task used to ask the gate, and read the rules, on its
// own (the store-loop guard's red on the approvals head). Who and required
// fields still say as the gate says each, the first refusal refuses the
// batch before anything is written, and a batch whose statuses are all the
// default, or a data move's, reads no rules at all.
func TestABulkCreateReadsItsProjectsRulesOnce(t *testing.T) {
	g := &gateWorld{admin: true, rules: []*model.StatusRule{
		{Status: "done", Who: "admins"},
		{Status: "backlog", Who: "people", People: []string{uuid.NewString()}},
		{Status: "inReview", Who: "anyone", Required: []string{"due"}, Approvers: []string{uuid.NewString()}, ApprovalsNeeded: 1},
	}}
	g.use(t)
	reads, stood := 0, 0
	read, stand := rulesFor, standingOf
	rulesFor = func(ctx context.Context, id uuid.UUID, keys []string) ([]*model.StatusRule, error) {
		reads++
		return read(ctx, id, keys)
	}
	standingOf = func(ctx context.Context, by *dgraphStruct.DgraphUser, on *dgraphStruct.DgraphTask) (workAccess.Standing, error) {
		stood++
		return stand(ctx, by, on)
	}
	project := &dgraphStruct.DgraphProject{Uid: "0x1", Uuid: uuid.NewString(), Name: "Launch", Team: &dgraphStruct.DgraphTeam{Uid: "0x9"}}
	maker := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: uuid.MustParse(ben.Uuid)}, UserDgraphInfo: *ben}
	task := func(name, status, due string) taskBusiness.NewTask {
		return taskBusiness.NewTask{Input: adapter.CreateOrUpdateTaskInput{TaskName: name, ProjectUuid: project.Uuid, Status: status, DueDate: due}, Parent: -1}
	}
	create := func(ctx context.Context, batch ...taskBusiness.NewTask) error {
		reads, stood = 0, 0
		_, err := taskBusiness.CreateTasks(ctx, uuid.MustParse(project.Uuid), maker, project, batch)
		return err
	}
	ctx := context.Background()
	due := "2026-11-02T09:00:00Z"

	// Seven tasks over five statuses: In progress has no rule, In review wants
	// a due date and approval, Done is for the project's admins (ben is one),
	// and Backlog names someone else, so its task refuses the lot.
	err := create(ctx, task("Plan", "inProgress", ""), task("Review the post", "inReview", due), task("Note", "", ""),
		task("Ship", "done", ""), task("Review the deck", "inReview", due), task("Close", "done", ""), task("Park", "backlog", ""))
	if got := why(t, err); got != "Only the people this project names move tasks to Backlog." {
		t.Errorf("the batch is refused with %q", got)
	}
	if reads != 1 || stood != 1 {
		t.Errorf("a batch of 7 read the rules %d times and its maker's standing %d times, want once each", reads, stood)
	}
	// What a rule requires is read from each new task.
	if got := why(t, create(ctx, task("Plan", "inProgress", ""), task("Review the post", "inReview", ""))); got != "Give it a due date before moving it to In review." {
		t.Errorf("a task for In review without a due date: %q", got)
	}
	g.admin = false
	if got := why(t, create(ctx, task("Plan", "", ""), task("Ship", "done", ""))); got != "Only the project's admins move tasks to Done." {
		t.Errorf("a member's task for Done: %q", got)
	}
	// Asking nothing, they go on to their write, which has no database here.
	for name, batch := range map[string]struct {
		ctx   context.Context
		tasks []taskBusiness.NewTask
	}{
		"all in To do": {ctx, []taskBusiness.NewTask{task("Plan", "", ""), task("Note", "todo", "")}},
		"a data move":  {taskBusiness.DataMove(ctx), []taskBusiness.NewTask{task("Ship", "done", ""), task("Park", "backlog", "")}},
	} {
		if err := create(batch.ctx, batch.tasks...); !errors.Is(err, taskKeyModel.ErrNoDatabase) || reads != 0 {
			t.Errorf("%s: %v after %d reads of the rules, want no read", name, err, reads)
		}
	}
}
