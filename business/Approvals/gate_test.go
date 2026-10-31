package business

import (
	"context"
	"strings"
	"testing"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type gateWorld struct {
	rules    []*model.StatusRule
	admin    bool
	requests []*model.Approval
	told     int
	// blind are the people who can't see the task (WA2).
	blind map[string]bool
	// links are the project's client links that can comment; linkErr is
	// their read's answer (ErrPrivateTask for a task kept to some members).
	links   []string
	linkErr error
}

// seeAs lets everyone read the task but the blind, by name "Lee" for them.
func seeAs(t *testing.T, blind func() map[string]bool) {
	oldA, oldR := actorOf, readersOf
	t.Cleanup(func() { actorOf, readersOf = oldA, oldR })
	actorOf = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id.String(), UserName: "Lee"}}, nil
	}
	readersOf = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = !blind()[id]
		}
		return out, nil
	}
}

func (g *gateWorld) use(t *testing.T) {
	oldR, oldQ, oldS, oldA, oldL := rulesFor, request, standingOf, afterRequest, clientLinks
	t.Cleanup(func() { rulesFor, request, standingOf, afterRequest, clientLinks = oldR, oldQ, oldS, oldA, oldL })
	seeAs(t, func() map[string]bool { return g.blind })
	clientLinks = func(context.Context, uuid.UUID, uuid.UUID) ([]string, error) { return g.links, g.linkErr }
	rulesFor = func(_ context.Context, _ uuid.UUID, keys []string) ([]*model.StatusRule, error) {
		var out []*model.StatusRule
		for _, r := range g.rules {
			for _, k := range keys {
				if r.Status == k {
					out = append(out, r)
				}
			}
		}
		return out, nil
	}
	request = func(_ context.Context, a *model.Approval) (*model.Approval, bool, error) {
		for _, r := range g.requests {
			if r.TaskID == a.TaskID {
				return r, false, nil
			}
		}
		a.ID = uuid.New()
		g.requests = append(g.requests, a)
		return a, true, nil
	}
	standingOf = func(context.Context, *dgraphStruct.DgraphUser, *dgraphStruct.DgraphTask) (workAccess.Standing, error) {
		if g.admin {
			return workAccess.Standing{Role: workAccess.RoleAdmin}, nil
		}
		return workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	afterRequest = func(context.Context, *model.Approval, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser, string) {
		g.told++
	}
}

var done = taskStatusBusiness.Resolved{Category: "done"}
var todo = taskStatusBusiness.Resolved{Category: "todo"}

func newTask() *dgraphStruct.DgraphTask {
	return &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: "Ship it", Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString()}}
}

var ben = &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), Uid: "0x2"}

func why(t *testing.T, err error) string {
	t.Helper()
	r, ok := taskBusiness.RefusedStatus(err)
	if !ok {
		t.Fatalf("not a refusal: %v", err)
	}
	return r.Why
}

// No rule, no say: the move goes.
func TestAStatusWithoutARuleTakesAnyMove(t *testing.T) {
	(&gateWorld{}).use(t)
	if err := gate(context.Background(), newTask(), todo, done, ben); err != nil {
		t.Fatal(err)
	}
}

// Who may move, what the task must have, in words; a custom status meets the
// rule of the built-in status it counts as, unless it has its own.
func TestAStatusRuleSaysWhoAndWhatInWords(t *testing.T) {
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "admins", Required: []string{"assignee", "due"}}}}
	g.use(t)
	task := newTask()
	if w := why(t, gate(context.Background(), task, todo, done, ben)); w != "Only the project's admins move tasks to Done." {
		t.Errorf("a member: %q", w)
	}
	g.admin = true
	if w := why(t, gate(context.Background(), task, todo, done, ben)); w != "Give it an assignee and a due date before moving it to Done." {
		t.Errorf("an admin, task without them: %q", w)
	}
	due := time.Now()
	task.Assignee, task.DueDate = &dgraphStruct.DgraphUser{Uuid: "x"}, &due
	if err := gate(context.Background(), task, todo, done, ben); err != nil {
		t.Errorf("all there: %v", err)
	}
	shipped := taskStatusBusiness.Resolved{Category: "done", CustomID: "ship-id", CustomName: "Shipped"}
	g.admin = false
	if w := why(t, gate(context.Background(), task, todo, shipped, ben)); !strings.Contains(w, "Shipped") {
		t.Errorf("a custom Done status falls under Done's rule: %q", w)
	}
	g.rules = append(g.rules, &model.StatusRule{Status: "ship-id", Who: "anyone"})
	if err := gate(context.Background(), task, todo, shipped, ben); err != nil {
		t.Errorf("its own rule wins: %v", err)
	}
	g.rules = []*model.StatusRule{{Status: "done", Who: "people", People: []string{"someone-else"}}}
	if w := why(t, gate(context.Background(), task, todo, done, ben)); !strings.Contains(w, "people this project names") {
		t.Errorf("named people: %q", w)
	}
}

// A move that needs approving becomes one request, and its approvers are
// told once; the approved move itself goes through.
func TestAMoveThatNeedsApprovalBecomesOneRequest(t *testing.T) {
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Approvers: []string{uuid.NewString(), uuid.NewString()}, ApprovalsNeeded: 2}}}
	g.use(t)
	task := newTask()
	r, _ := taskBusiness.RefusedStatus(gate(context.Background(), task, todo, done, ben))
	if r == nil || r.ApprovalID == "" || !strings.Contains(r.Why, "once 2 approvers say yes") {
		t.Fatalf("%+v", r)
	}
	again, _ := taskBusiness.RefusedStatus(gate(context.Background(), task, todo, done, ben))
	if again.ApprovalID != r.ApprovalID || !strings.Contains(again.Why, "waiting for approval") || len(g.requests) != 1 || g.told != 1 {
		t.Errorf("a second move: %+v requests %d told %d", again, len(g.requests), g.told)
	}
	if g.requests[0].Needed != 2 || g.requests[0].RequestedBy.String() != ben.Uuid || g.requests[0].ToStatus != "done" {
		t.Errorf("request %+v", g.requests[0])
	}
	if err := gate(withApproved(context.Background(), uuid.New()), task, todo, done, ben); err != nil {
		t.Errorf("the approved move: %v", err)
	}
}

// A status that wants only the client holds the move until a client link
// answers: with no link that can comment it says to share one, a private
// task can't be sent at all, and the request names the links and needs no
// member's yes.
func TestAMoveThatWantsOnlyTheClientWaitsForThem(t *testing.T) {
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Approvers: []string{}, ApprovalsNeeded: 1, Client: true}}}
	g.use(t)
	task := newTask()
	if w := why(t, gate(context.Background(), task, todo, done, ben)); w != "Moving it to Done needs the client's approval: share a client link that can comment first." {
		t.Errorf("no client link: %q", w)
	}
	g.linkErr = ErrPrivateTask
	if w := why(t, gate(context.Background(), task, todo, done, ben)); w != "Moving it to Done needs the client's approval, and this task is private to some members." {
		t.Errorf("a private task: %q", w)
	}
	g.linkErr, g.links = nil, []string{uuid.NewString()}
	r, _ := taskBusiness.RefusedStatus(gate(context.Background(), task, todo, done, ben))
	if r == nil || r.ApprovalID == "" || r.Why != "Sent for the client's approval: it moves to Done once they approve." {
		t.Fatalf("a link to answer: %+v", r)
	}
	if a := g.requests[0]; a.Needed != 0 || len(a.Approvers) != 0 || len(a.ClientGrants) != 1 || a.ClientGrants[0] != g.links[0] || g.told != 1 {
		t.Errorf("the request %+v, told %d", a, g.told)
	}
}

// A move the gate can't place (no project or task id it can read) or that
// names no one making it is held rather than let past the project's rules.
func TestAMoveTheGateCantPlaceIsHeld(t *testing.T) {
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "admins"}}}
	g.use(t)
	unplaced := map[string]*dgraphStruct.DgraphTask{
		"no project":           {Uuid: uuid.NewString(), Name: "Ship it"},
		"a project with no id": {Uuid: uuid.NewString(), Name: "Ship it", Project: &dgraphStruct.DgraphProject{Uid: "0x9"}},
		"a task with no id":    {Name: "Ship it", Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString()}},
	}
	for name, task := range unplaced {
		if w := why(t, gate(context.Background(), task, todo, done, ben)); w != cantCheck {
			t.Errorf("%s: %q", name, w)
		}
	}
	if w := why(t, gate(context.Background(), newTask(), todo, done, nil)); w != cantCheck {
		t.Errorf("no one making it: %q", w)
	}
}

// A rule is never lowered for approvers who can't answer: hiding the task
// from one, or one leaving, holds the move and says why, rather than let it
// through on fewer yeses, or on the client's alone.
func TestARuleIsNeverLoweredForApproversWhoCantAnswer(t *testing.T) {
	sees, blind, gone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Approvers: []string{sees, blind}, ApprovalsNeeded: 2}},
		blind: map[string]bool{blind: true}}
	g.use(t)
	here := actorOf
	actorOf = func(ctx context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		if id.String() == gone {
			return nil, userDomain.ErrNoActor
		}
		return here(ctx, id)
	}
	held := func(want string) {
		t.Helper()
		if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	held("Moving it to Done needs 2 approvals, and only 1 of its approvers can see this task.")
	g.rules[0].Client, g.links, g.blind[sees] = true, []string{uuid.NewString()}, true
	held("Moving it to Done needs approval, and none of its approvers can see this task.")
	g.rules[0] = &model.StatusRule{Status: "done", Who: "anyone", Approvers: []string{sees, gone}, ApprovalsNeeded: 2}
	g.blind = map[string]bool{}
	held("Moving it to Done needs 2 approvals, and only 1 of its approvers is still in the workspace.")
	g.rules[0].Approvers, g.rules[0].ApprovalsNeeded = []string{sees, uuid.NewString(), gone}, 3
	held("Moving it to Done needs 3 approvals, and only 2 of its approvers are still in the workspace.")
	g.rules[0].Approvers, g.rules[0].ApprovalsNeeded = []string{gone}, 1
	held("Moving it to Done needs approval, and its approvers have left the workspace.")
	if len(g.requests) != 0 || g.told != 0 {
		t.Errorf("requests %+v, told %d", g.requests, g.told)
	}
}
