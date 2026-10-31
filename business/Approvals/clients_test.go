package business

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// clientWorld answers a client's verdict on w.a, the item's open request.
func (w *decideWorld) clients(t *testing.T) {
	w.use(t)
	oldP, oldD, oldS := pendingForItem, decideAs, statusName
	t.Cleanup(func() { pendingForItem, decideAs, statusName = oldP, oldD, oldS })
	pendingForItem = func(_ context.Context, kind, id string) (*model.Approval, error) {
		if w.a.State != model.Pending || kind != w.a.ResourceKind || id != w.a.ResourceID {
			return nil, model.ErrNotFound
		}
		c := *w.a
		return &c, nil
	}
	decideAs = func(_ context.Context, _ uuid.UUID, by model.Decider, verdict, note string) (*model.Approval, error) {
		w.a.Decisions = append(w.a.Decisions, model.Decision{UserID: by.User, GrantID: by.Grant, Client: by.Grant != uuid.Nil, Name: by.Name, Verdict: verdict, Comment: note})
		c := *w.a
		return &c, nil
	}
	statusName = func(context.Context, uuid.UUID, string) string { return "Done" }
}

func clientRequest(needed int, approvers []string, grants ...uuid.UUID) *model.Approval {
	task := uuid.New()
	names := make([]string, len(grants))
	for i, g := range grants {
		names[i] = g.String()
	}
	return &model.Approval{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: task.String(), TaskID: task, ProjectID: uuid.New(), ToStatus: "done",
		RequestedBy: uuid.New(), Approvers: approvers, Needed: needed, ClientGrants: names, Client: len(names) > 0, State: model.Pending}
}

// A client's verdict counts only on a request that names the link it came
// through (DOCS-LEAD-2's condition 2); then it approves as a member would,
// and the held move is made as the person who asked.
func TestAClientsApprovalCountsOnlyOnARequestNamingTheirLink(t *testing.T) {
	ours, theirs := uuid.New(), uuid.New()
	w := &decideWorld{a: clientRequest(0, nil, ours)}
	w.clients(t)
	ctx := context.Background()
	clientVerdict(ctx, theirs, model.KindTask, w.a.ResourceID, "Ship it", "approved", "Dana", "")
	if len(w.a.Decisions) != 0 || w.a.State != model.Pending {
		t.Fatalf("another link's verdict counted: %+v", w.a)
	}
	clientVerdict(ctx, ours, model.KindTask, uuid.NewString(), "Other", "approved", "Dana", "")
	if len(w.a.Decisions) != 0 {
		t.Fatalf("a verdict on another item counted: %+v", w.a.Decisions)
	}
	clientVerdict(ctx, ours, model.KindTask, w.a.ResourceID, "Ship it", "approved", "Dana", "Lovely")
	if len(w.a.Decisions) != 1 || w.a.Decisions[0].GrantID != ours || w.a.Decisions[0].Name != "Dana" || w.a.State != model.Approved {
		t.Fatalf("the named link's approval: %+v", w.a)
	}
	if len(w.moved) != 1 || w.moved[0] != w.a.RequestedBy.String()+" done" {
		t.Errorf("moved %v", w.moved)
	}
	if len(w.told) != 1 || w.told[0] != "Ship it is approved and moved to Done" {
		t.Errorf("told %v", w.told)
	}
	if len(w.events) != 1 || w.events[0]["outcome"] != model.Approved || w.events[0]["approver"] != "Dana (client)" || w.events[0]["task_id"] != w.a.TaskID.String() {
		t.Errorf("rules heard %v", w.events)
	}
}

// A request that wants its approvers and the client waits for both.
func TestARequestThatWantsTheClientWaitsForThem(t *testing.T) {
	member, link := person(), uuid.New()
	w := &decideWorld{a: clientRequest(1, []string{member.UserPostgresInfo.Id.String()}, link)}
	w.clients(t)
	ctx := context.Background()
	if v, err := Decide(ctx, member, w.a.TaskID, w.a.ID, model.VerdictApprove, ""); err != nil || v.State != model.Pending || len(w.moved) != 0 {
		t.Fatalf("the member alone: %+v %v moved %v", v, err, w.moved)
	}
	clientVerdict(ctx, link, model.KindTask, w.a.ResourceID, "Ship it", "approved", "", "")
	if w.a.State != model.Approved || len(w.moved) != 1 {
		t.Fatalf("with the client too: %+v moved %v", w.a, w.moved)
	}
	if w.a.Decisions[1].Name != "The client" {
		t.Errorf("a client who gave no name: %q", w.a.Decisions[1].Name)
	}
}

// A client asking for changes closes the request, and the person who asked
// hears it in the client's name.
func TestAClientsChangesCloseTheRequest(t *testing.T) {
	link := uuid.New()
	w := &decideWorld{a: clientRequest(0, nil, link)}
	w.clients(t)
	clientVerdict(context.Background(), link, model.KindTask, w.a.ResourceID, "Ship it", "changes", "Dana", "The logo is too small")
	if w.a.State != model.Changes || len(w.moved) != 0 {
		t.Fatalf("%+v moved %v", w.a, w.moved)
	}
	if len(w.told) != 1 || w.told[0] != "Dana (client) asked for changes to moving Ship it to Done" {
		t.Errorf("told %v", w.told)
	}
	if len(w.events) != 1 || w.events[0]["outcome"] != model.Changes || w.events[0]["comment"] != "The logo is too small" {
		t.Errorf("rules heard %v", w.events)
	}
}

// A client's answer the database fails to keep is logged and changes
// nothing: the request still waits, and nothing moves or is told.
func TestAClientsAnswerThatFailsToSaveChangesNothing(t *testing.T) {
	link := uuid.New()
	w := &decideWorld{a: clientRequest(0, nil, link)}
	w.clients(t)
	decideAs = func(context.Context, uuid.UUID, model.Decider, string, string) (*model.Approval, error) {
		return nil, errors.New("connection reset by peer")
	}
	clientVerdict(context.Background(), link, model.KindTask, w.a.ResourceID, "Ship it", "approved", "Dana", "")
	if w.a.State != model.Pending || len(w.a.Decisions) != 0 || len(w.moved) != 0 || len(w.told) != 0 || len(w.events) != 0 {
		t.Errorf("%+v moved %v told %v events %v", w.a, w.moved, w.told, w.events)
	}
}

// A request about the item itself is approved without a move.
func TestAnItemsRequestIsApprovedWithoutAMove(t *testing.T) {
	member := person()
	w := &decideWorld{a: clientRequest(1, []string{member.UserPostgresInfo.Id.String()})}
	w.a.ToStatus = ""
	w.use(t)
	v, err := Decide(context.Background(), member, w.a.TaskID, w.a.ID, model.VerdictApprove, "")
	if err != nil || v.State != model.Approved || len(w.moved) != 0 {
		t.Fatalf("%+v %v moved %v", v, err, w.moved)
	}
	if len(w.told) != 1 || w.told[0] != "Ship it is approved" {
		t.Errorf("told %v", w.told)
	}
}

// A link's client is asked what waits for them in its project, by task: a
// held move says where it goes by the project's name for the status, an ask
// keeps its words and due time, and who asked is read once for all of them,
// as a client may be shown them (never their address).
func TestALinkIsAskedWhatWaitsForItsClient(t *testing.T) {
	grant, project := uuid.New(), uuid.New()
	ana, ben := uuid.New(), uuid.New()
	move, ask := uuid.New(), uuid.New()
	signOff := uuid.NewString()
	due := time.Date(2026, 10, 30, 11, 30, 0, 0, time.UTC)
	oldL, oldN, oldP := forLink, statusNames, peopleOf
	t.Cleanup(func() { forLink, statusNames, peopleOf = oldL, oldN, oldP })
	forLink = func(_ context.Context, p, g uuid.UUID) ([]*model.Approval, error) {
		if p != project || g != grant {
			t.Errorf("read project %s's requests for link %s", p, g)
		}
		return []*model.Approval{
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: move.String(), TaskID: move, ProjectID: project, ToStatus: signOff, RequestedBy: ana},
			{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: ask.String(), TaskID: ask, ProjectID: project, Ask: "The layout", DueAt: &due, RequestedBy: ben},
		}, nil
	}
	statusNames = func(_ context.Context, p uuid.UUID) (func(string) string, error) {
		return func(v string) string {
			if v == signOff {
				return "Client sign-off"
			}
			return ""
		}, nil
	}
	reads := 0
	peopleOf = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		reads++
		if len(ids) != 2 {
			t.Errorf("asked who %v are", ids)
		}
		// ben can't be read: his ask still shows, by nobody named.
		return []*dgraphStruct.DgraphUser{{Uuid: ana.String(), UserName: "ana", EmailID: "ana@acme.test"}}, nil
	}
	got, err := clientAsks(context.Background(), grant, project)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(got) != 2 {
		t.Fatalf("%d reads of who asked, %+v", reads, got)
	}
	if a := got[move.String()]; a.ToName != "Client sign-off" || a.Ask != "" || a.AskedBy == nil || a.AskedBy.UserName != "ana" || a.AskedBy.EmailID != "" {
		t.Errorf("the held move: %+v", a)
	}
	if a := got[ask.String()]; a.Ask != "The layout" || a.ToName != "" || a.DueAt == nil || !a.DueAt.Equal(due) || a.AskedBy != nil {
		t.Errorf("the ask: %+v", a)
	}
}

// A link nothing waits for costs its client's page one read; a failed read
// is said, not taken for nothing asked.
func TestALinkNothingWaitsForReadsNothingMore(t *testing.T) {
	oldL, oldN, oldP := forLink, statusNames, peopleOf
	t.Cleanup(func() { forLink, statusNames, peopleOf = oldL, oldN, oldP })
	statusNames = func(context.Context, uuid.UUID) (func(string) string, error) {
		t.Error("read the project's statuses")
		return nil, nil
	}
	peopleOf = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) {
		t.Error("read who asked")
		return nil, nil
	}
	forLink = func(context.Context, uuid.UUID, uuid.UUID) ([]*model.Approval, error) { return nil, nil }
	if got, err := clientAsks(context.Background(), uuid.New(), uuid.New()); err != nil || len(got) != 0 {
		t.Errorf("nothing waiting: %+v %v", got, err)
	}
	down := errors.New("postgres is down")
	forLink = func(context.Context, uuid.UUID, uuid.UUID) ([]*model.Approval, error) { return nil, down }
	if _, err := clientAsks(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, down) {
		t.Errorf("a failed read: %v", err)
	}
}
