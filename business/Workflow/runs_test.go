package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// fakeStore stands in for the rules' tables, the task and the people.
type fakeStore struct {
	mu        sync.Mutex
	runs      map[string]*workflowModel.Run // by event key
	ops       []string
	generated []bool
	paused    []string
	told      []string
	task      *dgraphStruct.DgraphTask
	hidden    bool
	recent    int // runs a count answers
	members   map[string]bool
	turn      int64
	counts    map[string]int
	keptOut   map[string]bool // people the task's restriction keeps out
}

func newStore(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{runs: map[string]*workflowModel.Run{}, members: map[string]bool{}, counts: map[string]int{}, keptOut: map[string]bool{},
		task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: "Fix SSO", Priority: "low", Status: "todo",
			Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString()}, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "creator"}}}
	type saved struct {
		claim   func(context.Context, *workflowModel.Run, string) (bool, error)
		finish  func(context.Context, *workflowModel.Run) error
		since   func(context.Context, *uuid.UUID, time.Time) (int, error)
		pause   func(context.Context, uuid.UUID, string) (bool, error)
		record  func(context.Context, uuid.UUID, time.Time, string) error
		actor   func(context.Context, string) (*userModels.UserInfo, error)
		read    func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphTask, error)
		apply   func(context.Context, *userModels.UserInfo, uuid.UUID, []taskWriter.Op) error
		members func(context.Context, string, *userModels.UserInfo) (map[string]bool, error)
		counts  func(context.Context, string) (map[string]int, error)
		turn    func(context.Context, uuid.UUID) (int64, error)
		tell    func(context.Context, notificationBusiness.Notice, []notificationBusiness.NoticeRecipient) []string
		reload  func(context.Context) error
		access  ownerAccess
		readers func(context.Context, string, []string) (map[string]bool, error)
	}
	old := saved{claimRun, finishRun, runsSince, pauseRule, recordRun, loadActor, readTask, applyOps, membersOf, openCounts, nextTurn, tell, reloadRules, access, readersOf}
	t.Cleanup(func() {
		claimRun, finishRun, runsSince, pauseRule, recordRun = old.claim, old.finish, old.since, old.pause, old.record
		loadActor, readTask, applyOps, membersOf, openCounts, nextTurn, tell, reloadRules, access = old.actor, old.read, old.apply, old.members, old.counts, old.turn, old.tell, old.reload, old.access
		readersOf = old.readers
	})
	claimRun = func(_ context.Context, r *workflowModel.Run, key string) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.runs[key]; ok {
			return false, nil
		}
		if r.ID == uuid.Nil {
			r.ID = uuid.New()
		}
		f.runs[key] = r
		return true, nil
	}
	finishRun = func(context.Context, *workflowModel.Run) error { return nil }
	runsSince = func(context.Context, *uuid.UUID, time.Time) (int, error) { return f.recent, nil }
	pauseRule = func(_ context.Context, id uuid.UUID, reason string) (bool, error) {
		f.paused = append(f.paused, reason)
		return len(f.paused) == 1, nil
	}
	recordRun = func(context.Context, uuid.UUID, time.Time, string) error { return nil }
	loadActor = func(_ context.Context, id string) (*userModels.UserInfo, error) {
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id, Uid: "0x1"}}, nil
	}
	readTask = func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphTask, error) {
		if f.hidden {
			return nil, errors.New("not visible")
		}
		c := *f.task
		return &c, nil
	}
	applyOps = func(ctx context.Context, who *userModels.UserInfo, _ uuid.UUID, ops []taskWriter.Op) error {
		for _, op := range ops {
			f.ops = append(f.ops, who.UserDgraphInfo.Uuid+" "+string(op.Kind)+" "+string(op.Value))
			f.generated = append(f.generated, helpers.IsWorkflowGenerated(ctx))
			var v string
			_ = json.Unmarshal(op.Value, &v)
			switch op.Kind {
			case taskWriter.OpPriority:
				f.task.Priority = v
			case taskWriter.OpAssignee:
				if v == "" {
					f.task.Assignee = nil
				} else {
					f.task.Assignee = &dgraphStruct.DgraphUser{Uuid: v}
				}
			}
		}
		return nil
	}
	membersOf = func(context.Context, string, *userModels.UserInfo) (map[string]bool, error) { return f.members, nil }
	openCounts = func(context.Context, string) (map[string]int, error) { return f.counts, nil }
	nextTurn = func(context.Context, uuid.UUID) (int64, error) { f.turn++; return f.turn, nil }
	tell = func(_ context.Context, n notificationBusiness.Notice, rs []notificationBusiness.NoticeRecipient) []string {
		for _, r := range rs {
			f.told = append(f.told, r.UserUUID+": "+n.Title)
		}
		return nil
	}
	reloadRules = func(context.Context) error { return nil }
	readersOf = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = !f.keptOut[id]
		}
		return out, nil
	}
	access = ownerAccess{
		readsChannel: func(context.Context, string, string) (bool, error) { return true, nil },
		inProject:    func(context.Context, string, string) (bool, error) { return true, nil },
	}
	return f
}

func rule(conds Conditions, then, otherwise []WorkflowAction) *compiledWorkflow {
	return &compiledWorkflow{id: uuid.New(), name: "Triage", createdBy: "owner", triggerType: workflowModel.TriggerTaskCreated,
		conditions: conds, actions: then, elseActions: otherwise}
}

func (f *fakeStore) event(key string) triggerEvent {
	return triggerEvent{taskID: f.task.Uuid, projectID: f.task.Project.Uuid, eventKey: key, targetUserUUID: "mover",
		vars: map[string]string{"task": f.task.Name}}
}

// A rule runs as its owner, under the mark that keeps its writes from
// triggering rules, and once however many times its event is delivered.
func TestARuleRunsOncePerEventAsItsOwner(t *testing.T) {
	f := newStore(t)
	cw := rule(Conditions{}, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, nil)
	runWorkflow(context.Background(), cw, f.event("e1"))
	f.task.Priority = "low" // someone set it back; the same event comes again
	runWorkflow(context.Background(), cw, f.event("e1"))
	if len(f.ops) != 1 || f.ops[0] != `owner priority "high"` {
		t.Fatalf("ops %v", f.ops)
	}
	if !f.generated[0] {
		t.Error("a rule's write wasn't marked as the rule's")
	}
	r := f.runs["e1"]
	if r.Status != workflowModel.RunOK || r.Branch != "then" || len(r.Steps) != 1 {
		t.Fatalf("run %+v", r)
	}
	if s := r.Steps[0]; s.Field != "priority" || string(s.Before) != `"low"` || string(s.After) != `"high"` {
		t.Errorf("step %+v: it must keep what it replaced", s)
	}
}

// Conditions choose the branch; a branch with nothing in it leaves no run.
func TestConditionsChooseThenOrOtherwise(t *testing.T) {
	f := newStore(t)
	high := Conditions{Items: []Condition{{Field: "priority", Op: "is", Value: "high"}}}
	cw := rule(high, []WorkflowAction{{Type: ActionSetDue, DueInDays: days(2)}},
		[]WorkflowAction{{Type: ActionSetPriority, Priority: "medium"}})
	runWorkflow(context.Background(), cw, f.event("e1"))
	if r := f.runs["e1"]; r == nil || r.Branch != "else" || f.ops[0] != `owner priority "medium"` {
		t.Fatalf("the otherwise branch: %+v %v", r, f.ops)
	}
	only := rule(high, []WorkflowAction{{Type: ActionSetDue, DueInDays: days(2)}}, nil)
	runWorkflow(context.Background(), only, f.event("e2"))
	if _, ok := f.runs["e2"]; ok {
		t.Error("conditions that didn't hold, with no otherwise, recorded a run")
	}
}

// An owner who can't see the task does nothing to it, and the history says so.
func TestAnOwnerWhoCantSeeTheTaskIsSkipped(t *testing.T) {
	f := newStore(t)
	f.hidden = true
	runWorkflow(context.Background(), rule(Conditions{}, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, nil), f.event("e1"))
	r := f.runs["e1"]
	if r == nil || r.Status != workflowModel.RunSkipped || !strings.Contains(r.Error, "can't see") || len(f.ops) != 0 {
		t.Fatalf("%+v %v", r, f.ops)
	}
}

// A rule's notice reaches only people who may open the task now: one the
// task keeps out isn't told, whoever the rule names, and a rule left with
// nobody to tell says so; a read that fails tells no one.
func TestARulesNoticeReachesOnlyPeopleWhoMayOpenTheTask(t *testing.T) {
	f := newStore(t)
	f.keptOut["ben"] = true
	tellThem := []WorkflowAction{{Type: ActionNotify, Notify: NotifyPeople, UserIDs: []string{"ben", "cy", "cy"}, Text: "{task} is new"}}
	runWorkflow(context.Background(), rule(Conditions{}, tellThem, nil), f.event("e1"))
	if len(f.told) != 1 || f.told[0] != "cy: Fix SSO is new" {
		t.Fatalf("told %v, want cy alone, once", f.told)
	}
	f.keptOut["cy"] = true
	runWorkflow(context.Background(), rule(Conditions{}, tellThem, nil), f.event("e2"))
	if r := f.runs["e2"]; len(f.told) != 1 || r == nil || r.Steps[0].Status != "skipped" {
		t.Fatalf("with everyone kept out: told %v, run %+v", f.told, r)
	}
	readersOf = func(context.Context, string, []string) (map[string]bool, error) { return nil, errors.New("down") }
	runWorkflow(context.Background(), rule(Conditions{}, tellThem, nil), f.event("e3"))
	if r := f.runs["e3"]; len(f.told) != 1 || r == nil || r.Steps[0].Status != "failed" {
		t.Fatalf("with the read down: told %v, run %+v", f.told, r)
	}
}

// A rule that runs away stops itself and its owner is told, once; the
// workspace's own limit skips without stopping anyone's rule.
func TestARunawayRuleStopsItself(t *testing.T) {
	f := newStore(t)
	f.recent = perMinute
	cw := rule(Conditions{}, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, nil)
	runWorkflow(context.Background(), cw, f.event("e1"))
	runWorkflow(context.Background(), cw, f.event("e2"))
	if len(f.ops) != 0 {
		t.Fatalf("a rule over its limit acted: %v", f.ops)
	}
	if len(f.told) != 1 || !strings.Contains(f.told[0], "owner: ") || !strings.Contains(f.told[0], "stopped itself") {
		t.Fatalf("told %v", f.told)
	}
	if r := f.runs["e1"]; r.Status != workflowModel.RunSkipped || !strings.HasPrefix(r.Error, "Paused") {
		t.Errorf("run %+v", r)
	}

	g := newStore(t)
	old := perMinute
	perMinute, perHour = 1<<30, 1<<30
	defer func() { perMinute, perHour = old, 1000 }()
	g.recent = workspacePerHour
	runWorkflow(context.Background(), cw, g.event("e3"))
	if len(g.paused) != 0 || len(g.ops) != 0 || !strings.Contains(g.runs["e3"].Error, "workspace") {
		t.Errorf("workspace limit: paused %v ops %v run %+v", g.paused, g.ops, g.runs["e3"])
	}
}

// Round robin goes round the people it names who are still in the project;
// least loaded picks whoever has fewest open tasks, counted as the rule's
// owner sees the project.
func TestAssignByTurnsAndByLoad(t *testing.T) {
	f := newStore(t)
	var countedAs []string
	openCounts = func(ctx context.Context, _ string) (map[string]int, error) {
		countedAs = append(countedAs, taskread.ReaderOf(ctx))
		return f.counts, nil
	}
	f.members = map[string]bool{"ana": false, "ben": false, "cy": false, "bot": true}
	cw := rule(Conditions{}, []WorkflowAction{{Type: ActionAssign, Assign: AssignRoundRobin, UserIDs: []string{"cy", "ana", "gone"}}}, nil)
	var got []string
	for i := 0; i < 3; i++ {
		f.task.Assignee = nil
		runWorkflow(context.Background(), cw, f.event("rr"+string(rune('0'+i))))
		got = append(got, f.task.Assignee.Uuid)
	}
	if strings.Join(got, ",") != "ana,cy,ana" {
		t.Errorf("turns %v: the named people still in the project, in order", got)
	}
	f.counts = map[string]int{"ana": 4, "ben": 1, "cy": 2}
	f.task.Assignee = nil
	runWorkflow(context.Background(), rule(Conditions{}, []WorkflowAction{{Type: ActionAssign, Assign: AssignLeastLoaded}}, nil), f.event("ll"))
	if f.task.Assignee.Uuid != "ben" {
		t.Errorf("least loaded gave it to %s", f.task.Assignee.Uuid)
	}
	// The fake owner has a profile and no Postgres row: read by their
	// profile's uuid, as a signed-in person is.
	if strings.Join(countedAs, ",") != "owner" {
		t.Errorf("open tasks counted as %q, want as the rule's owner", countedAs)
	}
	f.counts = map[string]int{"ana": 1, "ben": 1, "cy": 1}
	f.task.Assignee = nil
	runWorkflow(context.Background(), rule(Conditions{}, []WorkflowAction{{Type: ActionAssign, Assign: AssignLeastLoaded}}, nil), f.event("ll2"))
	if f.task.Assignee.Uuid == "bot" {
		t.Error("everyone never includes a bot")
	}
}

// Undo puts back what a run changed, as the person undoing it, and keeps a
// change someone made since.
func TestUndoPutsBackOnlyWhatTheRuleStillHolds(t *testing.T) {
	f := newStore(t)
	cw := rule(Conditions{}, []WorkflowAction{
		{Type: ActionSetPriority, Priority: "high"},
		{Type: ActionAssign, Assign: AssignPerson, UserIDs: []string{"ana"}},
	}, nil)
	runWorkflow(context.Background(), cw, f.event("e1"))
	run := f.runs["e1"]
	run.ProjectID = ptr(uuid.MustParse(f.task.Project.Uuid))
	f.task.Assignee = &dgraphStruct.DgraphUser{Uuid: "ben"} // someone reassigned it since
	oldGet, oldMark := getRun, markUndone
	defer func() { getRun, markUndone = oldGet, oldMark }()
	getRun = func(context.Context, uuid.UUID) (*workflowModel.Run, error) { return run, nil }
	var marked []workflowModel.Step
	markUndone = func(_ context.Context, _, _ uuid.UUID, steps []workflowModel.Step) (bool, error) {
		marked = steps
		return true, nil
	}
	f.ops = nil
	undoer := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "undoer"}}
	if _, err := UndoRun(context.Background(), undoer, *run.ProjectID, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 1 || f.ops[0] != `undoer priority "low"` {
		t.Fatalf("undo made %v", f.ops)
	}
	if marked[0].Status != "undone" || marked[1].Status == "undone" || !strings.Contains(marked[1].Detail, "changed since") {
		t.Errorf("steps %+v", marked)
	}
	if _, err := UndoRun(context.Background(), undoer, uuid.New(), run.ID); !IsNotThisProject(err) {
		t.Errorf("another project's run: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// Rules are never set through a token.
func TestRulesAreNotSetThroughAToken(t *testing.T) {
	newStore(t)
	ctx := helpers.WithActsThroughToken(context.Background())
	who := &userModels.UserInfo{}
	if _, err := CreateProjectRule(ctx, who, uuid.New(), WorkflowInput{Name: "x", TriggerType: workflowModel.TriggerTaskCreated,
		Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}); !errors.Is(err, ErrThroughToken) {
		t.Errorf("create: %v", err)
	}
	if err := SetProjectRuleActive(ctx, who, uuid.New(), uuid.New(), true); !errors.Is(err, ErrThroughToken) {
		t.Errorf("turn on: %v", err)
	}
	if _, err := UndoRun(ctx, who, uuid.New(), uuid.New()); !errors.Is(err, ErrThroughToken) {
		t.Errorf("undo: %v", err)
	}
	if _, err := CreateWorkflow(ctx, WorkflowInput{Name: "x"}, uuid.New()); !errors.Is(err, ErrThroughToken) {
		t.Errorf("workspace create: %v", err)
	}
}

// A project rule whose owner left the project says so in its history; a
// channel workflow whose owner can't read the channel hears nothing.
func TestAnOwnerWhoLeftIsSaidInTheHistory(t *testing.T) {
	f := newStore(t)
	access = ownerAccess{
		readsChannel: func(context.Context, string, string) (bool, error) { return false, nil },
		inProject:    func(context.Context, string, string) (bool, error) { return false, nil },
	}
	runWorkflow(context.Background(), rule(Conditions{}, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, nil), f.event("e1"))
	if r := f.runs["e1"]; r == nil || r.Status != workflowModel.RunSkipped || !strings.Contains(r.Error, "isn't in the project") || len(f.ops) != 0 {
		t.Fatalf("%+v %v", r, f.ops)
	}
	chat := &compiledWorkflow{id: uuid.New(), createdBy: "owner", triggerType: workflowModel.TriggerMessagePosted, actions: []WorkflowAction{{Type: ActionReply, Text: "hi"}}}
	runWorkflow(context.Background(), chat, triggerEvent{channelID: "c", eventKey: "e2"})
	if _, ok := f.runs["e2"]; ok {
		t.Error("a channel its owner can't read left a run")
	}
}
