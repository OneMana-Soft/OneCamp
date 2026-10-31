package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// askWorld is a project with members, client links, and the requests opened.
type askWorld struct {
	members []string
	links   []string
	linkErr error
	blind   map[string]bool
	opened  []*model.Approval
	need    workAccess.Need
	said    []string
	asked   int
}

func (w *askWorld) use(t *testing.T) {
	oldF, oldM, oldL, oldO, oldC, oldA, oldN := taskFor, membersAmong, clientLinks, openAppr, comment, afterRequest, now
	t.Cleanup(func() {
		taskFor, membersAmong, clientLinks, openAppr, comment, afterRequest, now = oldF, oldM, oldL, oldO, oldC, oldA, oldN
	})
	project := uuid.NewString()
	taskFor = func(_ context.Context, id uuid.UUID, _ *userModels.UserInfo, need workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		w.need = need
		return &dgraphStruct.DgraphTask{Uuid: id.String(), Name: "Ship it", Project: &dgraphStruct.DgraphProject{Uuid: project}}, workAccess.Standing{Role: workAccess.RoleMember}, nil
	}
	membersAmong = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		in := map[string]bool{}
		for _, id := range ids {
			for _, m := range w.members {
				if m == id {
					in[id] = true
				}
			}
		}
		return in, nil
	}
	clientLinks = func(context.Context, uuid.UUID, uuid.UUID) ([]string, error) { return w.links, w.linkErr }
	seeAs(t, func() map[string]bool { return w.blind })
	openAppr = func(_ context.Context, a *model.Approval) (*model.Approval, bool, error) {
		for _, o := range w.opened {
			if o.ResourceKind == a.ResourceKind && o.ResourceID == a.ResourceID {
				return o, false, nil
			}
		}
		a.ID, a.State = uuid.New(), model.Pending
		a.Client = len(a.ClientGrants) > 0
		w.opened = append(w.opened, a)
		return a, true, nil
	}
	comment = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, body string) (string, error) {
		w.said = append(w.said, body)
		return "c", nil
	}
	afterRequest = func(context.Context, *model.Approval, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser, string) {
		w.asked++
	}
	now = func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) }
}

// Asking for a task itself to be approved: whoever may change it asks its
// approvers among the project's members, N of them, and its clients through
// the project's comment links; one open request at a time.
func TestAskingForATaskItselfToBeApproved(t *testing.T) {
	ana, ben := person(), person()
	w := &askWorld{members: []string{ana.UserPostgresInfo.Id.String(), ben.UserPostgresInfo.Id.String()}}
	w.use(t)
	ctx := context.Background()
	task := uuid.New()
	ask := func(in AskInput) (*View, error) { return AskForTask(ctx, ana, task, in) }

	if _, err := AskForTask(helpers.WithActsThroughToken(ctx), ana, task, AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}}); !errors.Is(err, ErrThroughToken) {
		t.Errorf("through a token: %v", err)
	}
	if _, err := ask(AskInput{}); !errors.Is(err, ErrNobodyToAsk) {
		t.Errorf("nobody named: %v", err)
	}
	if _, err := ask(AskInput{Approvers: []string{uuid.NewString()}}); err == nil {
		t.Error("an approver outside the project was asked")
	}
	if _, err := ask(AskInput{Client: true}); !errors.Is(err, ErrNoClientLink) {
		t.Errorf("the client, with no link: %v", err)
	}
	past := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if _, err := ask(AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}, DueAt: &past}); !errors.Is(err, ErrDuePassed) {
		t.Errorf("due in the past: %v", err)
	}
	if _, err := ask(AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}, Ask: strings.Repeat("x", MaxAsk+1)}); err == nil {
		t.Error("a too long ask was taken")
	}
	if len(w.opened) != 0 {
		t.Fatalf("a refused ask opened %d requests", len(w.opened))
	}

	w.links = []string{uuid.NewString()}
	due := time.Date(2026, 10, 24, 17, 0, 0, 0, time.UTC)
	v, err := ask(AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String(), ben.UserPostgresInfo.Id.String()}, Needed: 5, Client: true, DueAt: &due, Ask: "  The banner, v3  "})
	if err != nil {
		t.Fatal(err)
	}
	if w.need != workAccess.NeedEditTask {
		t.Errorf("asked with %v", w.need)
	}
	a := w.opened[0]
	if a.ResourceKind != model.KindTask || a.ResourceID != task.String() || a.ToStatus != "" || a.Needed != 1 || len(a.Approvers) != 1 ||
		len(a.ClientGrants) != 1 || a.Ask != "The banner, v3" || !a.DueAt.Equal(due) || a.RequestedBy != ana.UserPostgresInfo.Id {
		t.Errorf("opened %+v", a)
	}
	if !v.Client || v.State != model.Pending {
		t.Errorf("view %+v", v)
	}
	if len(w.said) != 1 || w.said[0] != "Asked for approval. The banner, v3" || w.asked != 1 {
		t.Errorf("said %v, approvers told %d", w.said, w.asked)
	}
	if _, err := ask(AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}}); !errors.Is(err, ErrWaitingAlready) {
		t.Errorf("a second ask: %v", err)
	}
}

// A status rule can want the client: a move into it opens a request that
// names the project's client links, and with none it says what to do.
func TestAStatusRuleCanWantTheClient(t *testing.T) {
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Client: true}}}
	g.use(t)
	old := clientLinks
	t.Cleanup(func() { clientLinks = old })
	var links []string
	clientLinks = func(context.Context, uuid.UUID, uuid.UUID) ([]string, error) { return links, nil }
	task := newTask()
	if got := why(t, gate(context.Background(), task, todo, done, ben)); got != "Moving it to Done needs the client's approval: share a client link that can comment first." {
		t.Errorf("with no link: %q", got)
	}
	links = []string{uuid.NewString()}
	if got := why(t, gate(context.Background(), task, todo, done, ben)); got != "Sent for the client's approval: it moves to Done once they approve." {
		t.Errorf("with a link: %q", got)
	}
	if got := sentFor("Done", 2, true); got != "Sent for approval: it moves to Done once 2 approvers and the client say yes." {
		t.Errorf("approvers and the client: %q", got)
	}
	if len(g.requests) != 1 || len(g.requests[0].ClientGrants) != 1 || g.requests[0].Needed != 0 {
		t.Errorf("requests %+v", g.requests)
	}
}

// Only those who can see a task are asked about it (PROJ-LEAD-4): an
// approver it's kept from is named in the refusal, and no client link sees a
// task kept to some members.
func TestOnlyThoseWhoCanSeeATaskAreAskedAboutIt(t *testing.T) {
	ana, ben := person(), person()
	w := &askWorld{members: []string{ana.UserPostgresInfo.Id.String(), ben.UserPostgresInfo.Id.String()}, blind: map[string]bool{ben.UserPostgresInfo.Id.String(): true}}
	w.use(t)
	ctx := context.Background()
	_, err := AskForTask(ctx, ana, uuid.New(), AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}})
	if err == nil || err.Error() != "Lee can't see this task, so can't approve it" {
		t.Errorf("an approver who can't see it: %v", err)
	}
	w.linkErr = ErrPrivateTask
	if _, err := AskForTask(ctx, ana, uuid.New(), AskInput{Client: true}); !errors.Is(err, ErrPrivateTask) {
		t.Errorf("the client, on a private task: %v", err)
	}
	if len(w.opened) != 0 {
		t.Errorf("opened %+v", w.opened)
	}
}

// A held move asks only the rule's approvers who can see the task and says
// who isn't asked; with none who can, and no client, it says so; and a
// private task never waits for a client.
func TestAHeldMoveAsksOnlyThoseWhoCanSeeTheTask(t *testing.T) {
	sees, blind := uuid.NewString(), uuid.NewString()
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Approvers: []string{sees, blind}, ApprovalsNeeded: 1}}, blind: map[string]bool{blind: true}}
	g.use(t)
	if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != "Sent for approval: it moves to Done once it's approved. Lee can't see this task, so isn't asked." {
		t.Errorf("one of two can see it: %q", got)
	}
	if len(g.requests) != 1 || len(g.requests[0].Approvers) != 1 || g.requests[0].Approvers[0] != sees || g.requests[0].Needed != 1 {
		t.Errorf("requests %+v", g.requests)
	}
	g.blind[sees] = true
	if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != "Moving it to Done needs approval, and none of its approvers can see this task." {
		t.Errorf("none can see it: %q", got)
	}
	g.rules[0].Client, g.linkErr = true, ErrPrivateTask
	if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != "Moving it to Done needs the client's approval, and this task is private to some members." {
		t.Errorf("a private task that wants the client: %q", got)
	}
}

// An approver who has left isn't asked or named, two who can't see the
// task are named in a sentence that reads right, and a person's read that
// fails holds the move rather than drop an approver who is still here.
func TestOnlyApproversStillHereWhoCanSeeTheTaskAreAsked(t *testing.T) {
	here, gone, lee, dana := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	g := &gateWorld{rules: []*model.StatusRule{{Status: "done", Who: "anyone", Approvers: []string{gone, lee, here, dana}, ApprovalsNeeded: 1}},
		blind: map[string]bool{lee: true, dana: true}}
	g.use(t)
	names := map[string]string{here: "Sam", lee: "Lee", dana: "Dana"}
	var failed error
	actorOf = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		switch {
		case id.String() == gone:
			return nil, userDomain.ErrNoActor
		case id.String() == here && failed != nil:
			return nil, failed
		}
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id.String(), UserName: names[id.String()]}}, nil
	}
	if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != "Sent for approval: it moves to Done once it's approved. Lee and Dana can't see this task, so aren't asked." {
		t.Errorf("one has left, two can't see it: %q", got)
	}
	if len(g.requests) != 1 || len(g.requests[0].Approvers) != 1 || g.requests[0].Approvers[0] != here {
		t.Fatalf("requests %+v", g.requests)
	}
	failed = errors.New("connection reset by peer")
	if got := why(t, gate(context.Background(), newTask(), todo, done, ben)); got != "Couldn't ask for approval. Try again in a moment." || len(g.requests) != 1 {
		t.Errorf("a read that failed: %q, requests %d", got, len(g.requests))
	}
}

// clientLinks names none for a task kept to some members.
func TestNoClientLinkSeesAPrivateTask(t *testing.T) {
	old := restrictionOf
	t.Cleanup(func() { restrictionOf = old })
	restrictionOf = func(context.Context, uuid.UUID) (restrictionModel.Restriction, error) {
		return restrictionModel.Restriction{Restricted: true}, nil
	}
	if links, err := clientLinks(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, ErrPrivateTask) || len(links) != 0 {
		t.Errorf("%v %v", links, err)
	}
}
