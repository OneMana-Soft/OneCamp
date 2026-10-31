package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// withAgentHooks sets the AI edition's hooks for one test.
func withAgentHooks(t *testing.T, a AgentAsker, h AgentHandOff) {
	t.Helper()
	agentMu.Lock()
	oldA, oldH := agentAsker, agentHandOff
	agentAsker, agentHandOff = a, h
	agentMu.Unlock()
	t.Cleanup(func() {
		agentMu.Lock()
		agentAsker, agentHandOff = oldA, oldH
		agentMu.Unlock()
	})
}

// Without the AI edition, "ask an agent" can't be saved, and one saved
// before fails saying why.
func TestAskingAnAgentNeedsTheAIEdition(t *testing.T) {
	withAgentHooks(t, nil, nil)
	act := WorkflowAction{Type: ActionAskAgent, Text: "Is this a bug or a feature?"}
	if err := validateTaskAction(&act); !errors.Is(err, ErrNeedsAI) {
		t.Fatalf("saved without AI: %v", err)
	}
	f := newStore(t)
	cw := rule(Conditions{}, []WorkflowAction{act}, nil)
	step := cw.runTaskAction(context.Background(), act, f.event("k"), &runCtx{owner: &userModels.UserInfo{}, task: f.task}, 0)
	if step.Status != "failed" || !strings.Contains(step.Detail, "AI edition") {
		t.Errorf("step %+v, want failed for want of AI", step)
	}
}

// The answer is asked as the rule's owner and posted as the workspace's
// assistant, with the owner's right to comment; the history keeps its first
// line.
func TestAnAgentsAnswerIsPostedOnTheTask(t *testing.T) {
	f := newStore(t)
	var asked AgentQuestion
	var askedAs string
	bot := uuid.New()
	withAgentHooks(t, func(_ context.Context, owner *userModels.UserInfo, q AgentQuestion) (AgentAnswer, error) {
		asked, askedAs = q, owner.UserDgraphInfo.Uuid
		return AgentAnswer{Text: "**Bug.** The login fails after SSO.\n\n- Check the callback URL", By: bot, Name: "OneCamp AI"}, nil
	}, nil)
	oldActor, oldPost := actorOf, makeCommentAs
	t.Cleanup(func() { actorOf, makeCommentAs = oldActor, oldPost })
	actorOf = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		if id != bot {
			t.Errorf("posted as %s, want the one who answered", id)
		}
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "onecamp-ai"}}, nil
	}
	var who, by, body string
	makeCommentAs = func(_ context.Context, w, b *userModels.UserInfo, _ uuid.UUID, text string) (string, error) {
		who, by, body = w.UserDgraphInfo.Uuid, b.UserDgraphInfo.Uuid, text
		return "comment-1", nil
	}
	act := WorkflowAction{Type: ActionAskAgent, Text: "Is {task} a bug?"}
	cw := rule(Conditions{}, []WorkflowAction{act}, nil)
	owner := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "owner"}}
	step := cw.runTaskAction(context.Background(), act, f.event("k"), &runCtx{owner: owner, task: f.task}, 0)
	if step.Status != "ok" || step.Made != "comment-1" || step.Detail != "Bug. The login fails after SSO." {
		t.Fatalf("step %+v", step)
	}
	if askedAs != "owner" || asked.Prompt != "Is Fix SSO a bug?" || asked.Task.Uuid != f.task.Uuid || asked.Agent != "" {
		t.Errorf("asked %q as %q of %q", asked.Prompt, askedAs, asked.Agent)
	}
	if who != "owner" || by != "onecamp-ai" {
		t.Errorf("posted as %q with %q's right, want the assistant with the owner's", by, who)
	}
	if !strings.Contains(body, "<strong>Bug.</strong>") || !strings.Contains(body, "<li>") {
		t.Errorf("body %q, want the answer's Markdown as HTML", body)
	}
}

// A rule that gives a task to an AI teammate starts it, as assigning it by
// hand does; giving it to a person starts nothing.
func TestARuleHandsWorkToAnAITeammate(t *testing.T) {
	f := newStore(t)
	var handed []map[string]interface{}
	withAgentHooks(t, nil, func(_ context.Context, data map[string]interface{}) { handed = append(handed, data) })
	oldIs := isAgent
	t.Cleanup(func() { isAgent = oldIs })
	isAgent = func(_ context.Context, id string) bool { return id == "agent-1" }
	f.members = map[string]bool{"agent-1": true, "maya": false}

	toAgent := WorkflowAction{Type: ActionAssign, Assign: AssignPerson, UserIDs: []string{"agent-1"}}
	cw := rule(Conditions{}, []WorkflowAction{toAgent}, nil)
	owner := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "owner", Uid: "0x1"}}
	step := cw.runTaskAction(context.Background(), toAgent, f.event("k"), &runCtx{owner: owner, task: f.task}, 0)
	if step.Status != "ok" || len(handed) != 1 {
		t.Fatalf("step %+v, handed %d", step, len(handed))
	}
	if handed[0]["assignee_id"] != "agent-1" || handed[0]["task_id"] != f.task.Uuid || handed[0]["assigned_by"] != "owner" {
		t.Errorf("handed %v", handed[0])
	}
	toMaya := WorkflowAction{Type: ActionAssign, Assign: AssignPerson, UserIDs: []string{"maya"}}
	cw.runTaskAction(context.Background(), toMaya, f.event("k2"), &runCtx{owner: owner, task: f.task}, 0)
	if len(handed) != 1 {
		t.Errorf("a person was handed to an agent: %v", handed)
	}
}

// What a rule's steps may be depends on what they act on.
func TestStepsFitWhatTheRuleActsOn(t *testing.T) {
	pid := uuid.NewString()
	weekly := map[string]interface{}{"repeat": "FREQ=WEEKLY;BYDAY=MO", "at": "09:00"}
	each := map[string]interface{}{"repeat": "FREQ=WEEKLY;BYDAY=MO", "at": "09:00", "each_task": true}
	mk := WorkflowAction{Type: ActionCreateTask, ProjectID: pid, TaskName: "Weekly review"}
	assign := WorkflowAction{Type: ActionAssign, Assign: AssignNobody}
	high := Conditions{Items: []Condition{{Field: "priority", Op: "is", Value: "high"}}}
	cases := []struct {
		name string
		in   WorkflowInput
		ok   bool
	}{
		{"a schedule makes a task, then acts on it", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: weekly, Actions: []WorkflowAction{mk, assign}}, true},
		{"a schedule's step before the task", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: weekly, Actions: []WorkflowAction{assign, mk}}, false},
		{"a schedule making two tasks", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: weekly, Actions: []WorkflowAction{mk, mk}}, false},
		{"a schedule that makes a task with conditions", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: weekly, Conditions: high, Actions: []WorkflowAction{mk}}, false},
		{"each task with conditions", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: each, Conditions: high, Actions: []WorkflowAction{assign}}, true},
		{"each task with an otherwise", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: each, Actions: []WorkflowAction{assign}, ElseActions: []WorkflowAction{assign}}, false},
		{"each task making tasks", WorkflowInput{TriggerType: workflowModel.TriggerSchedule, TriggerConfig: each, Actions: []WorkflowAction{mk}}, false},
		{"overdue raises priority", WorkflowInput{TriggerType: workflowModel.TriggerTaskOverdue, Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}, true},
	}
	for _, c := range cases {
		in := c.in
		in.Name, in.ProjectID = "Rule", pid
		_, err := validate(&in)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	ws := WorkflowInput{Name: "Rule", TriggerType: workflowModel.TriggerSchedule, TriggerConfig: weekly, Actions: []WorkflowAction{mk}}
	if _, err := validate(&ws); err == nil {
		t.Error("a workspace workflow took a schedule")
	}
}

// A question to a named AI teammate becomes that teammate's work on the task:
// it answers there in its own name, and the rule posts nothing for it.
func TestARuleAsksANamedAITeammate(t *testing.T) {
	f := newStore(t)
	teammate := uuid.NewString()
	var asked AgentQuestion
	withAgentHooks(t, func(_ context.Context, _ *userModels.UserInfo, q AgentQuestion) (AgentAnswer, error) {
		asked = q
		return AgentAnswer{Queued: "job-1", Name: "Triage Bot"}, nil
	}, nil)
	oldPost := makeCommentAs
	t.Cleanup(func() { makeCommentAs = oldPost })
	makeCommentAs = func(context.Context, *userModels.UserInfo, *userModels.UserInfo, uuid.UUID, string) (string, error) {
		t.Error("a teammate's answer was posted by the rule")
		return "", nil
	}
	act := WorkflowAction{Type: ActionAskAgent, Text: "How bad is it?", AgentID: teammate}
	if err := validateTaskAction(&act); err != nil {
		t.Fatal(err)
	}
	step := rule(Conditions{}, []WorkflowAction{act}, nil).runTaskAction(context.Background(), act, f.event("k"),
		&runCtx{owner: &userModels.UserInfo{}, task: f.task}, 0)
	if asked.Agent != teammate || step.Status != "ok" || step.Made != "job-1" || step.Detail != "asked Triage Bot, who answers on the task" {
		t.Errorf("asked %q; step %+v", asked.Agent, step)
	}
	bad := WorkflowAction{Type: ActionAskAgent, Text: "?", AgentID: "not-a-person"}
	if err := validateTaskAction(&bad); err == nil {
		t.Error("an agent that isn't an id was saved")
	}
}

// A rule asks only an AI teammate: a person's id, sent as the agent, is
// refused when the rule is saved (never trust a client's ids).
func TestARuleAsksOnlyAnAITeammate(t *testing.T) {
	teammate, person := uuid.NewString(), uuid.NewString()
	oldIs := isAgent
	t.Cleanup(func() { isAgent = oldIs })
	isAgent = func(_ context.Context, id string) bool { return id == teammate }
	ctx := context.Background()
	if err := checkTaskActionsIn(ctx, uuid.NewString(), []WorkflowAction{{Type: ActionAskAgent, Text: "?", AgentID: person}}); err == nil {
		t.Error("a rule was saved asking a person as its agent")
	}
	if err := checkTaskActionsIn(ctx, uuid.NewString(), nil, []WorkflowAction{{Type: ActionAskAgent, Text: "?", AgentID: person}}); err == nil {
		t.Error("an otherwise step was saved asking a person as its agent")
	}
	if err := checkTaskActionsIn(ctx, uuid.NewString(), []WorkflowAction{{Type: ActionAskAgent, Text: "?", AgentID: teammate}, {Type: ActionAskAgent, Text: "?"}}); err != nil {
		t.Errorf("a rule asking a teammate and OneCamp AI was refused: %v", err)
	}
}

// A teammate already at work on the task isn't asked again (its open job
// would swallow the question): the run says so, and posts nothing.
func TestABusyTeammateIsNotAskedAgain(t *testing.T) {
	f := newStore(t)
	withAgentHooks(t, func(context.Context, *userModels.UserInfo, AgentQuestion) (AgentAnswer, error) {
		return AgentAnswer{Busy: true, Name: "Triage Bot"}, nil
	}, nil)
	oldPost := makeCommentAs
	t.Cleanup(func() { makeCommentAs = oldPost })
	makeCommentAs = func(context.Context, *userModels.UserInfo, *userModels.UserInfo, uuid.UUID, string) (string, error) {
		t.Error("a busy teammate's question was posted by the rule")
		return "", nil
	}
	act := WorkflowAction{Type: ActionAskAgent, Text: "How bad is it?", AgentID: uuid.NewString()}
	step := rule(Conditions{}, []WorkflowAction{act}, nil).runTaskAction(context.Background(), act, f.event("k"),
		&runCtx{owner: &userModels.UserInfo{}, task: f.task}, 0)
	if step.Status != "skipped" || step.Made != "" || step.Detail != "Triage Bot was already at work on this task, so it wasn't asked again" {
		t.Errorf("step %+v", step)
	}
}
