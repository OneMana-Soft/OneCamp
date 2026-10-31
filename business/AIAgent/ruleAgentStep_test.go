package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// withTeammate stands in for the agent a principal belongs to and the queue
// its work goes on.
func withTeammate(t *testing.T, agent *model.AiAgent, queue func(ctx context.Context, a *model.AiAgent, taskID, prompt string, by *uuid.UUID) (uuid.UUID, bool, error)) {
	t.Helper()
	oldFor, oldQueue := agentByPrincipal, queueWork
	t.Cleanup(func() { agentByPrincipal, queueWork = oldFor, oldQueue })
	agentByPrincipal = func(context.Context, string) (*model.AiAgent, error) { return agent, nil }
	queueWork = queue
}

// A rule's question to an AI teammate becomes the teammate's work on the
// task, started by the rule's owner, and asks it the question in its words.
func TestARuleQuestionBecomesTheTeammatesWork(t *testing.T) {
	owner := &userModels.UserInfo{}
	owner.UserPostgresInfo.Id = uuid.New()
	desc := "<p>Crashes on <b>save</b></p>"
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: "Fix the crash", Description: &desc}
	job := uuid.New()
	var gotTask, gotPrompt string
	var gotBy *uuid.UUID
	withTeammate(t, &model.AiAgent{Id: uuid.New(), Name: "Triage Bot", IsActive: true},
		func(_ context.Context, _ *model.AiAgent, taskID, prompt string, by *uuid.UUID) (uuid.UUID, bool, error) {
			gotTask, gotPrompt, gotBy = taskID, prompt, by
			return job, true, nil
		})
	ans, err := askTeammate(context.Background(), owner, workflowBusiness.AgentQuestion{Prompt: "How bad is it?", Task: task, Agent: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Queued != job.String() || ans.Name != "Triage Bot" || ans.Busy || ans.Text != "" {
		t.Errorf("answer %+v", ans)
	}
	if gotTask != task.Uuid || gotBy == nil || *gotBy != owner.UserPostgresInfo.Id {
		t.Errorf("queued on %q by %v", gotTask, gotBy)
	}
	for _, want := range []string{"How bad is it?", "Task: Fix the crash", "Crashes on save", "task_uuid: " + task.Uuid} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, gotPrompt)
		}
	}
}

// A teammate with open work on the task isn't asked again: the queue keeps
// one open job a task, so the question would vanish into it unanswered.
func TestATeammateAtWorkIsNotAskedAgain(t *testing.T) {
	withTeammate(t, &model.AiAgent{Id: uuid.New(), Name: "Triage Bot", IsActive: true},
		func(context.Context, *model.AiAgent, string, string, *uuid.UUID) (uuid.UUID, bool, error) {
			return uuid.New(), false, nil
		})
	ans, err := askTeammate(context.Background(), &userModels.UserInfo{}, workflowBusiness.AgentQuestion{Prompt: "?", Task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}, Agent: uuid.NewString()})
	if err != nil || !ans.Busy || ans.Queued != "" || ans.Name != "Triage Bot" {
		t.Errorf("answer %+v, err %v", ans, err)
	}
}

// A teammate switched off, or a principal that is no agent's, takes no work.
func TestAGoneTeammateTakesNoQuestion(t *testing.T) {
	for _, agent := range []*model.AiAgent{nil, {Id: uuid.New(), Name: "Paused", IsActive: false}} {
		withTeammate(t, agent, func(context.Context, *model.AiAgent, string, string, *uuid.UUID) (uuid.UUID, bool, error) {
			t.Error("work was queued for a teammate that isn't there")
			return uuid.Nil, false, nil
		})
		_, err := askTeammate(context.Background(), &userModels.UserInfo{}, workflowBusiness.AgentQuestion{Prompt: "?", Task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}, Agent: uuid.NewString()})
		if !errors.Is(err, errAgentGone) {
			t.Errorf("agent %+v: err %v, want errAgentGone", agent, err)
		}
	}
}

// A task filed from outside (a public form, a GitHub comment) carries a
// visitor's words: a rule asking a teammate about it records no asker, as an
// assignment from outside doesn't, so no tool runs with the rule owner's reach
// on a stranger's say.
func TestAQuestionAboutATaskFiledFromOutsideHasNoAsker(t *testing.T) {
	owner := &userModels.UserInfo{}
	owner.UserPostgresInfo.Id = uuid.New()
	asked := false
	var by *uuid.UUID
	withTeammate(t, &model.AiAgent{Id: uuid.New(), Name: "Triage Bot", IsActive: true},
		func(_ context.Context, _ *model.AiAgent, _, _ string, b *uuid.UUID) (uuid.UUID, bool, error) {
			asked, by = true, b
			return uuid.New(), true, nil
		})
	ctx := helpers.WithPublicSubmission(context.Background())
	if _, err := askTeammate(ctx, owner, workflowBusiness.AgentQuestion{Prompt: "?", Task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}, Agent: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if !asked || by != nil {
		t.Errorf("asked=%v by %v, want asked by nobody", asked, by)
	}
}
