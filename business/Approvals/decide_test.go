package business

import (
	"context"
	"errors"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type decideWorld struct {
	a       *model.Approval
	moved   []string
	said    []string
	told    []string
	events  []map[string]interface{}
	moveErr error
	// unreadable is who may no longer read the task (readersOf).
	unreadable map[string]bool
}

func person() *userModels.UserInfo {
	id := uuid.New()
	return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id.String(), UserName: id.String()[:4]}}
}

func (w *decideWorld) use(t *testing.T) {
	oldF, oldG, oldD, oldC, oldA, oldM, oldCo, oldT, oldR, oldRe := taskFor, getAppr, decide, closeAppr, actorOf, moveTask, comment, tell, tellRules, readersOf
	t.Cleanup(func() {
		taskFor, getAppr, decide, closeAppr, actorOf, moveTask, comment, tell, tellRules, readersOf = oldF, oldG, oldD, oldC, oldA, oldM, oldCo, oldT, oldR, oldRe
	})
	readersOf = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = !w.unreadable[id]
		}
		return out, nil
	}
	tellRules = func(_ context.Context, data map[string]interface{}) { w.events = append(w.events, data) }
	taskFor = func(context.Context, uuid.UUID, *userModels.UserInfo, workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		return &dgraphStruct.DgraphTask{Uuid: w.a.TaskID.String(), Name: "Ship it"}, workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	getAppr = func(context.Context, uuid.UUID) (*model.Approval, error) { c := *w.a; return &c, nil }
	decide = func(_ context.Context, _ uuid.UUID, user uuid.UUID, verdict, note string) (*model.Approval, error) {
		w.a.Decisions = append(w.a.Decisions, model.Decision{UserID: user, Verdict: verdict, Comment: note})
		c := *w.a
		return &c, nil
	}
	closeAppr = func(_ context.Context, _ uuid.UUID, state string) (bool, error) {
		if w.a.State != model.Pending {
			return false, nil
		}
		w.a.State = state
		return true, nil
	}
	actorOf = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}}, nil
	}
	moveTask = func(ctx context.Context, who *userModels.UserInfo, _ uuid.UUID, status string) error {
		if !approvedMove(ctx) {
			t.Error("the approved move wasn't marked approved")
		}
		w.moved = append(w.moved, who.UserPostgresInfo.Id.String()+" "+status)
		return w.moveErr
	}
	comment = func(_ context.Context, who *userModels.UserInfo, _ uuid.UUID, body string) (string, error) {
		w.said = append(w.said, body)
		return "c", nil
	}
	tell = func(_ context.Context, n notificationBusiness.Notice, _ []notificationBusiness.NoticeRecipient) []string {
		w.told = append(w.told, n.Title)
		return nil
	}
}

func TestEnoughApprovalsMakeTheMoveAsTheRequester(t *testing.T) {
	a1, a2, asker := person(), person(), person()
	w := &decideWorld{a: &model.Approval{ID: uuid.New(), TaskID: uuid.New(), ToStatus: "done", RequestedBy: asker.UserPostgresInfo.Id,
		Approvers: []string{a1.UserPostgresInfo.Id.String(), a2.UserPostgresInfo.Id.String()}, Needed: 2, State: model.Pending}}
	w.use(t)
	ctx := context.Background()
	v, err := Decide(ctx, a1, w.a.TaskID, w.a.ID, model.VerdictApprove, "")
	if err != nil || v.State != model.Pending || len(w.moved) != 0 {
		t.Fatalf("one of two: %+v %v moved %v", v, err, w.moved)
	}
	if v, err = Decide(ctx, a2, w.a.TaskID, w.a.ID, model.VerdictApprove, "Looks right"); err != nil || v.State != model.Approved {
		t.Fatalf("two of two: %+v %v", v, err)
	}
	if len(w.moved) != 1 || w.moved[0] != asker.UserPostgresInfo.Id.String()+" done" {
		t.Errorf("moved %v", w.moved)
	}
	if len(w.said) != 2 || w.said[1] != "Approved moving it to Done. Looks right" {
		t.Errorf("the task's history says %v", w.said)
	}
	if len(w.told) != 1 || w.told[0] != "Ship it is approved and moved to Done" {
		t.Errorf("told %v", w.told)
	}
	if _, err := Decide(ctx, a1, w.a.TaskID, w.a.ID, model.VerdictApprove, ""); !errors.Is(err, ErrNotOpen) {
		t.Errorf("answering a closed request: %v", err)
	}
}

func TestOneNoClosesTheRequest(t *testing.T) {
	a1, asker := person(), person()
	w := &decideWorld{a: &model.Approval{ID: uuid.New(), TaskID: uuid.New(), ToStatus: "done", RequestedBy: asker.UserPostgresInfo.Id,
		Approvers: []string{a1.UserPostgresInfo.Id.String()}, Needed: 1, State: model.Pending}}
	w.use(t)
	ctx := context.Background()
	if _, err := Decide(ctx, a1, w.a.TaskID, w.a.ID, model.VerdictChanges, ""); !errors.Is(err, ErrNeedsComment) {
		t.Errorf("changes without saying what: %v", err)
	}
	if _, err := Decide(ctx, person(), w.a.TaskID, w.a.ID, model.VerdictApprove, ""); !errors.Is(err, ErrNotApprover) {
		t.Errorf("someone not asked: %v", err)
	}
	if _, err := Decide(helpers.WithActsThroughToken(ctx), a1, w.a.TaskID, w.a.ID, model.VerdictApprove, ""); !errors.Is(err, ErrThroughToken) {
		t.Errorf("through a token: %v", err)
	}
	v, err := Decide(ctx, a1, w.a.TaskID, w.a.ID, model.VerdictChanges, "The copy needs the new price")
	if err != nil || v.State != model.Changes || len(w.moved) != 0 {
		t.Fatalf("%+v %v %v", v, err, w.moved)
	}
	if len(w.told) != 1 {
		t.Errorf("the asker heard %v", w.told)
	}
}

// A request is read only by those who can see its task, answered only
// through its own task by an approver who still sees it, and taken back only
// by whoever asked or someone who manages the project, never through a token.
func TestARequestIsOnlyItsPeoplesToReadAnswerOrTakeBack(t *testing.T) {
	approver, asker, member, manager, stranger := person(), person(), person(), person(), person()
	w := &decideWorld{a: &model.Approval{ID: uuid.New(), TaskID: uuid.New(), ToStatus: "done", RequestedBy: asker.UserPostgresInfo.Id,
		Approvers: []string{approver.UserPostgresInfo.Id.String(), stranger.UserPostgresInfo.Id.String()}, Needed: 1, State: model.Pending}}
	w.use(t)
	oldP := pendingFor
	t.Cleanup(func() { pendingFor = oldP })
	pendingFor = func(context.Context, uuid.UUID) (*model.Approval, error) { c := *w.a; return &c, nil }
	taskFor = func(_ context.Context, _ uuid.UUID, who *userModels.UserInfo, _ workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		switch who {
		case stranger:
			return nil, workAccess.Standing{}, taskBusiness.ErrTaskNotVisible
		case manager:
			return &dgraphStruct.DgraphTask{Uuid: w.a.TaskID.String()}, workAccess.Standing{Role: workAccess.RoleAdmin}, nil
		}
		return &dgraphStruct.DgraphTask{Uuid: w.a.TaskID.String()}, workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	ctx := context.Background()
	if _, err := ForTask(ctx, stranger, w.a.TaskID); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("someone the task is kept from reads its request: %v", err)
	}
	if _, err := Decide(ctx, stranger, w.a.TaskID, w.a.ID, model.VerdictApprove, ""); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("an approver who can no longer see the task answers: %v", err)
	}
	if _, err := Decide(ctx, approver, uuid.New(), w.a.ID, model.VerdictApprove, ""); !errors.Is(err, ErrNotOpen) {
		t.Errorf("a request answered through another task: %v", err)
	}
	// Each taking-back leg starts from a request still waiting, so a leg
	// above that let someone answer it can't hide one below behind "answered
	// already".
	waiting := func() { w.a.State, w.a.Decisions = model.Pending, nil }
	waiting()
	if err := Withdraw(ctx, member, w.a.TaskID); !errors.Is(err, ErrNotYours) {
		t.Errorf("a member who neither asked nor manages the project takes it back: %v", err)
	}
	waiting()
	if err := Withdraw(helpers.WithActsThroughToken(ctx), asker, w.a.TaskID); !errors.Is(err, ErrThroughToken) {
		t.Errorf("taken back through a token: %v", err)
	}
	waiting()
	if err := Withdraw(ctx, manager, w.a.TaskID); err != nil || w.a.State != model.Withdrawn {
		t.Errorf("someone who manages the project takes it back: %v, %s", err, w.a.State)
	}
}

// An approved move the asker can no longer make says why, and isn't made.
func TestAnApprovedMoveThatCantBeMadeSaysWhy(t *testing.T) {
	a1, asker := person(), person()
	w := &decideWorld{a: &model.Approval{ID: uuid.New(), TaskID: uuid.New(), ToStatus: "done", RequestedBy: asker.UserPostgresInfo.Id,
		Approvers: []string{a1.UserPostgresInfo.Id.String()}, Needed: 1, State: model.Pending}, moveErr: errors.New("you can't change this task")}
	w.use(t)
	if _, err := Decide(context.Background(), a1, w.a.TaskID, w.a.ID, model.VerdictApprove, ""); err != nil {
		t.Fatal(err)
	}
	if len(w.told) != 1 || w.told[0] != "Ship it is approved, but it couldn't move to Done: you can't change this task" {
		t.Errorf("told %v", w.told)
	}
}

// The person who asked hears how it ended only while they may read the task:
// taken off its project since they asked, they're told nothing of it.
func TestADecisionIsToldOnlyToARequesterWhoMayStillReadTheTask(t *testing.T) {
	a1, asker := person(), person()
	w := &decideWorld{a: &model.Approval{ID: uuid.New(), TaskID: uuid.New(), RequestedBy: asker.UserPostgresInfo.Id,
		Approvers: []string{a1.UserPostgresInfo.Id.String()}, Needed: 1, State: model.Pending},
		unreadable: map[string]bool{asker.UserPostgresInfo.Id.String(): true}}
	w.use(t)
	v, err := Decide(context.Background(), a1, w.a.TaskID, w.a.ID, model.VerdictApprove, "")
	if err != nil || v.State != model.Approved {
		t.Fatalf("approving: %+v %v", v, err)
	}
	if len(w.told) != 0 {
		t.Errorf("told %v, want nothing: the person who asked may no longer read the task", w.told)
	}
}
