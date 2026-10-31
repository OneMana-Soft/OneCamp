package business

import (
	"context"
	"testing"

	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The waiting list reads each item as its reader: a task they can't see
// never shows, and a held move says where it goes.
func TestWaitingShowsOnlyWhatTheReaderCanSee(t *testing.T) {
	me := person()
	seen, hidden := uuid.New(), uuid.New()
	oldW, oldF, oldS, oldP := waitingFor, taskFor, statusName, peopleOf
	t.Cleanup(func() { waitingFor, taskFor, statusName, peopleOf = oldW, oldF, oldS, oldP })
	peopleOf = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) { return nil, nil }
	var asked uuid.UUID
	waitingFor = func(_ context.Context, user uuid.UUID, limit int) ([]*model.Approval, error) {
		asked = user
		if limit != WaitingLimit {
			t.Errorf("limit %d", limit)
		}
		return []*model.Approval{
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: seen.String(), TaskID: seen, ToStatus: "done"},
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: hidden.String(), TaskID: hidden},
		}, nil
	}
	taskFor = func(_ context.Context, id uuid.UUID, _ *userModels.UserInfo, need workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		if need != workAccess.NeedView {
			t.Errorf("read with %v", need)
		}
		if id == hidden {
			return nil, workAccess.Standing{}, taskBusiness.ErrTaskNotVisible
		}
		return &dgraphStruct.DgraphTask{Uuid: id.String(), Name: "Ship it"}, workAccess.Standing{}, nil
	}
	statusName = func(context.Context, uuid.UUID, string) string { return "Done" }
	got, err := WaitingFor(context.Background(), me)
	if err != nil {
		t.Fatal(err)
	}
	if asked != me.UserPostgresInfo.Id {
		t.Errorf("asked for %v", asked)
	}
	if len(got) != 1 || got[0].Title != "Ship it" || got[0].Path != "/app/task/"+seen.String() || got[0].ToName != "Done" || got[0].Kind != model.KindTask {
		t.Fatalf("%+v", got)
	}
}

// Home draws each row with who asked (a bot as its square) and the task's
// project: the askers come in one read, however many rows there are.
func TestWaitingNamesWhoAskedAndTheProject(t *testing.T) {
	me := person()
	ana, bot := uuid.New(), uuid.New()
	first, second := uuid.New(), uuid.New()
	oldW, oldF, oldS, oldP := waitingFor, taskFor, statusName, peopleOf
	t.Cleanup(func() { waitingFor, taskFor, statusName, peopleOf = oldW, oldF, oldS, oldP })
	waitingFor = func(context.Context, uuid.UUID, int) ([]*model.Approval, error) {
		return []*model.Approval{
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: first.String(), TaskID: first, RequestedBy: ana},
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: second.String(), TaskID: second, RequestedBy: bot},
		}, nil
	}
	taskFor = func(_ context.Context, id uuid.UUID, _ *userModels.UserInfo, _ workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		return &dgraphStruct.DgraphTask{Uuid: id.String(), Name: "Ship it", Project: &dgraphStruct.DgraphProject{Uuid: "p1", Name: "Website"}}, workAccess.Standing{}, nil
	}
	statusName = func(context.Context, uuid.UUID, string) string { return "" }
	reads := 0
	peopleOf = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		reads++
		if len(ids) != 2 {
			t.Errorf("asked for %v", ids)
		}
		return []*dgraphStruct.DgraphUser{
			{Uuid: ana.String(), UserName: "ana", EmailID: "ana@acme.test"},
			{Uuid: bot.String(), UserName: "Triage bot", IsBot: true},
		}, nil
	}
	got, err := WaitingFor(context.Background(), me)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(got) != 2 {
		t.Fatalf("%d reads, %+v", reads, got)
	}
	if a := got[0].Asker; a == nil || a.UserName != "ana" || a.IsBot || a.EmailID != "" {
		t.Errorf("ana's row: %+v", a)
	}
	if a := got[1].Asker; a == nil || !a.IsBot {
		t.Errorf("the bot's row: %+v", a)
	}
	if got[0].ProjectID != "p1" || got[0].ProjectName != "Website" {
		t.Errorf("the project: %+v", got[0])
	}
}
