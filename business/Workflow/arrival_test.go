package business

import (
	"context"
	"strings"
	"testing"

	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// decided is what the triage action asked the queue: as whom, of which task,
// and the choice.
type decided struct {
	who    string
	task   uuid.UUID
	choice triageBusiness.Choice
}

// withArrival is newStore with the queue's decision and the rota's on-duty
// read answered.
func withArrival(t *testing.T) (*fakeStore, *[]decided, *string, *error) {
	t.Helper()
	f := newStore(t)
	var asked []decided
	onDuty, decideErr := "", error(nil)
	decide0, duty0 := decideAs, onDutyIn
	t.Cleanup(func() { decideAs, onDutyIn = decide0, duty0 })
	decideAs = func(_ context.Context, who *userModels.UserInfo, task uuid.UUID, c triageBusiness.Choice) error {
		asked = append(asked, decided{who: who.UserDgraphInfo.Uuid, task: task, choice: c})
		return decideErr
	}
	onDutyIn = func(context.Context, string) (string, error) { return onDuty, nil }
	return f, &asked, &onDuty, &decideErr
}

func arrivalRule(conds Conditions, then ...WorkflowAction) *compiledWorkflow {
	cw := rule(conds, then, nil)
	cw.triggerType = workflowModel.TriggerTaskArrived
	return cw
}

func (f *fakeStore) arrival(key, source string) triggerEvent {
	ev := f.event(key)
	ev.vars["source"] = source
	return ev
}

// task_arrived is a task trigger the engine hears, from Triage's event.
func TestTaskArrivedIsAnEngineTrigger(t *testing.T) {
	if !IsEngineTrigger(workflowModel.TriggerTaskArrived) || !isTaskTrigger(workflowModel.TriggerTaskArrived) {
		t.Fatal("task_arrived isn't heard")
	}
	if triageBusiness.ArrivedEvent != "task.arrived" {
		t.Fatalf("the event is %q", triageBusiness.ArrivedEvent)
	}
}

// The source condition tests the door work came in by: one of Triage's
// Sources, is or is not; anything else isn't saved.
func TestSourceConditionMatchesTheDoor(t *testing.T) {
	for _, ok := range []Condition{{Field: "source", Op: "is", Value: "form"}, {Field: "source", Op: "is_not", Value: "github"}} {
		c := Conditions{Items: []Condition{ok}}
		if err := c.validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []Condition{{Field: "source", Op: "is", Value: "fax"}, {Field: "source", Op: "contains", Value: "form"}, {Field: "source", Op: "is"},
		// An import never reaches the rules: it runs as bulk work, which the
		// engine doesn't hear, so a rule on it would save and never run.
		{Field: "source", Op: "is", Value: "import"}, {Field: "source", Op: "is_not", Value: "import"}} {
		c := Conditions{Items: []Condition{bad}}
		if err := c.validate(); err == nil {
			t.Errorf("%+v was saved", bad)
		}
	}
	f, _, _, _ := withArrival(t)
	fromAForm := Conditions{Items: []Condition{{Field: "source", Op: "is", Value: "form"}}}
	cw := arrivalRule(fromAForm, WorkflowAction{Type: ActionSetPriority, Priority: "high"})
	runWorkflow(context.Background(), cw, f.arrival("e1", "api"))
	if len(f.ops) != 0 {
		t.Fatalf("work from the API matched a form rule: %v", f.ops)
	}
	runWorkflow(context.Background(), cw, f.arrival("e2", "form"))
	if len(f.ops) != 1 {
		t.Fatalf("work from a form: %v", f.ops)
	}
}

// The triage action decides through the queue, the one decision path, as
// the rule's owner: accept into a status, or decline with the reason the
// requester is told. A decision the queue refuses fails the run, and the
// task is left to the queue (still waiting).
func TestTriageActionDecidesThroughTheQueueAsTheOwner(t *testing.T) {
	f, asked, _, decideErr := withArrival(t)
	accept := WorkflowAction{Type: ActionTriage, Decision: "accept", Status: "inProgress"}
	runWorkflow(context.Background(), arrivalRule(Conditions{}, accept), f.arrival("e1", "form"))
	task := uuid.MustParse(f.task.Uuid)
	if len(*asked) != 1 || (*asked)[0] != (decided{who: "owner", task: task, choice: triageBusiness.Choice{Decision: triageBusiness.Accept, Status: "inProgress"}}) {
		t.Fatalf("asked %+v", *asked)
	}
	if r := f.runs["e1"]; r.Status != workflowModel.RunOK || !strings.Contains(r.Steps[0].Detail, "accepted") {
		t.Fatalf("run %+v", r)
	}
	decline := WorkflowAction{Type: ActionTriage, Decision: "decline", Text: "We don't build plugins"}
	runWorkflow(context.Background(), arrivalRule(Conditions{}, decline), f.arrival("e2", "form"))
	if got := (*asked)[1].choice; got.Decision != triageBusiness.Decline || got.Reason != "We don't build plugins" {
		t.Fatalf("declined with %+v", got)
	}
	*decideErr = triageBusiness.ErrNotWaiting
	runWorkflow(context.Background(), arrivalRule(Conditions{}, accept), f.arrival("e3", "form"))
	if r := f.runs["e3"]; r.Status != workflowModel.RunFailed || r.Steps[0].Status != "failed" {
		t.Fatalf("a refused decision: run %+v", r)
	}
	for _, bad := range []WorkflowAction{
		{Type: ActionTriage},
		{Type: ActionTriage, Decision: "snooze"},
		{Type: ActionTriage, Decision: "decline"},
		{Type: ActionTriage, Decision: "decline", Text: strings.Repeat("x", triageBusiness.MaxReason+1)},
	} {
		if err := validateTaskAction(&bad); err == nil {
			t.Errorf("%+v was saved", bad)
		}
	}
}

// Deciding and the door are for arriving work: a rule on another trigger
// can't triage, nor test the door.
func TestTriageAndSourceAreForArrivingWorkOnly(t *testing.T) {
	triage := []WorkflowAction{{Type: ActionTriage, Decision: "accept"}}
	door := Conditions{Items: []Condition{{Field: "source", Op: "is", Value: "form"}}}
	if err := arrivalOnly(workflowModel.TriggerTaskArrived, door, triage); err != nil {
		t.Fatalf("an arrival rule: %v", err)
	}
	if err := arrivalOnly(workflowModel.TriggerTaskCreated, Conditions{}, triage); err == nil {
		t.Error("a task-created rule triages")
	}
	if err := arrivalOnly(workflowModel.TriggerTaskCreated, door, nil, triage); err == nil {
		t.Error("an otherwise branch triages")
	}
	if err := arrivalOnly(workflowModel.TriggerTaskStatusChanged, door); err == nil {
		t.Error("a status rule tests the door")
	}
}

// "on_duty" gives the task to, or tells, whoever the project's rota has on
// duty now; nobody on duty, or someone no longer in the project, fails the
// step and assigns no one.
func TestOnDutyTargetGoesToWhoeverIsOnDuty(t *testing.T) {
	f, _, onDuty, _ := withArrival(t)
	*onDuty = "ben"
	f.members["ben"] = false
	give := WorkflowAction{Type: ActionAssign, Assign: AssignOnDuty}
	tellThem := WorkflowAction{Type: ActionNotify, Notify: NotifyOnDuty, Text: "{task} arrived"}
	for _, a := range []WorkflowAction{give, tellThem} {
		if err := validateTaskAction(&a); err != nil {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	runWorkflow(context.Background(), arrivalRule(Conditions{}, give, tellThem), f.arrival("e1", "form"))
	if len(f.ops) != 1 || f.ops[0] != `owner assignee "ben"` || len(f.told) != 1 || f.told[0] != "ben: Fix SSO arrived" {
		t.Fatalf("ops %v, told %v", f.ops, f.told)
	}
	f.task.Assignee = nil
	*onDuty = ""
	runWorkflow(context.Background(), arrivalRule(Conditions{}, give), f.arrival("e2", "form"))
	if r := f.runs["e2"]; len(f.ops) != 1 || r.Steps[0].Status != "failed" || !strings.Contains(r.Steps[0].Detail, "triage duty") {
		t.Fatalf("nobody on duty: ops %v, run %+v", f.ops, r)
	}
	*onDuty = "cy" // on the rota, since gone from the project
	runWorkflow(context.Background(), arrivalRule(Conditions{}, give), f.arrival("e3", "form"))
	if r := f.runs["e3"]; len(f.ops) != 1 || r.Steps[0].Status != "failed" {
		t.Fatalf("someone gone: ops %v, run %+v", f.ops, r)
	}
}

// Triage's event runs the project's arrival rules, with the door in the
// rule's words ({source}), and no one else's.
func TestArrivingWorkRunsTheProjectsArrivalRules(t *testing.T) {
	f, _, _, _ := withArrival(t)
	mine := arrivalRule(Conditions{}, WorkflowAction{Type: ActionSetPriority, Priority: "high"})
	mine.projectID = f.task.Project.Uuid
	theirs := arrivalRule(Conditions{}, WorkflowAction{Type: ActionSetPriority, Priority: "urgent"})
	theirs.projectID = uuid.NewString()
	regMu.Lock()
	old := byTrigger
	byTrigger = map[string][]*compiledWorkflow{workflowModel.TriggerTaskArrived: {mine, theirs}}
	regMu.Unlock()
	t.Cleanup(func() { regMu.Lock(); byTrigger = old; regMu.Unlock() })
	handleEvent(context.Background(), triageBusiness.ArrivedEvent, map[string]interface{}{
		"event_id": uuid.NewString(), "task_id": f.task.Uuid, "project_id": f.task.Project.Uuid, "source": "form",
	})
	if len(f.ops) != 1 || f.ops[0] != `owner priority "high"` {
		t.Fatalf("ops %v", f.ops)
	}
	if vars := taskEventVars(map[string]interface{}{"source": "form"}); vars["source"] != "form" {
		t.Errorf("vars %v", vars)
	}
}
