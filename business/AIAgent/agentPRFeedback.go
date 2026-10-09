package business

// Feedback left on GitHub, on a pull request an agent's coding job opened.
//
// A GitHub commenter is nobody in OneCamp: on a public repository, anyone. Their
// comment used to continue every agent's work in the thread the pull request
// was posted to, as a message from nobody in particular, which the continuation
// engine let through: it answered a paused job, steered a running one, or
// started a follow-up that pushed to the pull request with the GitHub account of
// whoever asked for the change, without them knowing.
//
// It now reaches only the coding job of the agent that opened the pull request,
// and never while that job is in flight or waiting on its person. A change it
// asks for is proposed to the owner of the account the job pushes with, who sees
// what was asked and approves it or not; approved, it continues the same pull
// request (the follow-up shares the job's thread, so it builds on the earlier
// work), pushed as them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// ProposePullRequestFeedback proposes the change feedback asks for, left on a
// pull request agentID's coding job opened from the thread entityID, to the
// owner of the account that job pushes with. It reports whether it did.
func ProposePullRequestFeedback(ctx context.Context, agentID uuid.UUID, entityID, feedback string) bool {
	entityID = strings.TrimSpace(entityID)
	feedback = promptText(feedback)
	if agentID == uuid.Nil || entityID == "" || feedback == "" {
		return false
	}
	tasks, err := model.ListAgentTasksForEntity(ctx, entityID, candidateSourceIDs(entityID))
	if err != nil {
		return false
	}
	var job *model.AgentTask
	for _, t := range tasks { // newest first
		if t != nil && t.AgentId == agentID && t.SourceType == codepr.TaskSourceType {
			job = t
			break
		}
	}
	if job == nil {
		return false
	}
	switch job.State {
	case model.TaskAwaiting, model.TaskQueued, model.TaskRunning:
		helpers.LogInfoWithContext(ctx, "pull request feedback: job %s is under way or waiting on its person; not given the feedback", job.Id)
		return false
	}
	if job.RunAsUserId == nil || *job.RunAsUserId == uuid.Nil {
		return false
	}
	agent, err := model.GetAgentByID(ctx, agentID)
	if err != nil || agent == nil {
		return false
	}
	params := map[string]string{"instruction": feedback, ai.ProposalSurfaceParam: job.Surface}
	sum := sha256.Sum256([]byte(agentID.String() + "\n" + entityID + "\n" + feedback))
	if _, err := aiBusiness.CreatePendingAction(ctx, *job.RunAsUserId, "agent", agentID.String(), codePRToolName, params,
		prFeedbackDescription(agent.Name), "pr-feedback:"+hex.EncodeToString(sum[:16]), pendingModels.Attribution{AgentID: &agentID}); err != nil {
		helpers.LogErrorWithContext(ctx, "pull request feedback: proposing the change for job %s failed: %v", job.Id, err)
		return false
	}
	return true
}

// prFeedbackDescription is how the approval names the change.
func prFeedbackDescription(agentName string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "Agent"
	}
	return name + ": change the pull request it opened, as asked on GitHub"
}
