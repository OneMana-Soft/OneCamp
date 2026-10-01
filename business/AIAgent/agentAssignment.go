package business

// Assign-a-task-to-an-AI-teammate wiring. When a project task is assigned to an
// agent's bot principal, that is a durable hand-off of work: we enqueue an
// ai_agent_tasks job (agentTaskWorker drives it to completion, posting status
// back as task comments, surviving restarts and retrying transient failures).
//
// This rides the existing event bus exactly like the mention/event triggers, so
// there is no new coupling: business/Task dispatches "task.assigned" (it already
// imports the webhook bus); this listener resolves the assignee to an agent and
// enqueues. Loop safety is inherited — an agent's own writes run under
// helpers.WithWorkflowGenerated and DispatchEvent skips listeners for those, so
// an agent reassigning a task while it works can never re-enqueue itself.

import (
	"context"
	"fmt"
	"strings"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// AgentForBotPrincipal returns the active agent whose bot principal matches
// principalID (a user uuid or Dgraph uid), or (nil, nil) when none does — the
// common case for an ordinary human assignee. Only agents that ALREADY have a
// provisioned principal are considered, so evaluating an assignment never
// provisions a bot user for every agent.
func AgentForBotPrincipal(ctx context.Context, principalID string) (*model.AiAgent, error) {
	principalID = strings.TrimSpace(principalID)
	if principalID == "" {
		return nil, nil
	}
	// Candidate set: every agent that can legitimately be a task assignee /
	// addressable principal. An agent becomes assignable the moment it is
	// DM-able (that is what surfaces it in the task assignee picker), which can
	// happen BEFORE its bot_user_id is denormalized onto ai_agents — that column
	// is only persisted lazily, on a mention/schedule run or a channel-add. If
	// we resolved only against ListActiveWithBotPrincipal (bot_user_id NOT NULL),
	// a freshly-created DM-able agent could be assigned in the UI but never
	// resolved here, so the assignment would silently no-op and no durable task
	// would be enqueued. So union the live DM-able set with the
	// already-provisioned set, dedupe, and match.
	seen := map[uuid.UUID]bool{}
	var candidates []*model.AiAgent
	if dmAble, derr := model.ListDMable(ctx); derr == nil {
		for _, a := range dmAble {
			if a != nil && !seen[a.Id] {
				seen[a.Id] = true
				candidates = append(candidates, a)
			}
		}
	} else {
		helpers.LogErrorWithContext(ctx, "AgentForBotPrincipal list dm-able failed: %v", derr)
	}
	withPrincipal, err := model.ListActiveWithBotPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range withPrincipal {
		if a != nil && !seen[a.Id] {
			seen[a.Id] = true
			candidates = append(candidates, a)
		}
	}

	for _, a := range candidates {
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil {
			continue
		}
		if principalID == bot.UUID || principalID == bot.DgraphUID {
			// Self-heal: persist the denormalized link so subsequent lookups
			// find this agent in the fast bot_user_id set immediately.
			if a.BotUserId == nil || *a.BotUserId != bot.UserID {
				if serr := model.SetAgentBotUser(ctx, a.Id, bot.UserID); serr != nil {
					helpers.LogErrorWithContext(ctx, "AgentForBotPrincipal persist bot_user_id failed (agent=%s): %v", a.Id, serr)
				}
			}
			return a, nil
		}
	}
	return nil, nil
}

// handleTaskAssignedForAgent is the event-bus listener for "task.assigned". If
// the new assignee is an agent's bot principal it enqueues a durable agent task
// to work it; otherwise it is a no-op (ordinary human assignment). Inert when
// AI is disabled.
func handleTaskAssignedForAgent(ctx context.Context, eventType string, data map[string]interface{}) {
	if eventType != "task.assigned" {
		return
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	assigneeID, _ := data["assignee_id"].(string)
	taskID, _ := data["task_id"].(string)
	if strings.TrimSpace(assigneeID) == "" || strings.TrimSpace(taskID) == "" {
		return
	}
	agent, err := AgentForBotPrincipal(ctx, assigneeID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentAssignment: resolve assignee principal failed: %v", err)
		return
	}
	if agent == nil {
		return // assigned to a human; nothing to do
	}
	prompt := synthAssignmentPrompt(data)
	owner := agent.CreatedBy
	t := &model.AgentTask{
		AgentId:     agent.Id,
		SourceType:  model.TaskSourceAssignment,
		SourceId:    strings.TrimSpace(taskID),
		Prompt:      prompt,
		RunAsUserId: &owner,
	}
	// The person who assigned it is who started it: the audit says so, and it
	// is who hears when the agent needs a decision or has finished. The agent
	// still acts with its sponsor's access (RunAsUserId), never theirs.
	if by, perr := uuid.Parse(strings.TrimSpace(fmt.Sprint(data["assigned_by"]))); perr == nil && by != uuid.Nil {
		t.TriggeredBy = &by
	}
	id, created, eerr := model.EnqueueAgentTask(ctx, t)
	if eerr != nil {
		helpers.LogErrorWithContext(ctx, "agentAssignment: enqueue failed (agent=%s task=%s): %v", agent.Id, taskID, eerr)
		return
	}
	if created {
		helpers.LogInfoWithContext(ctx, "agentAssignment: queued agent task %s (agent=%s task=%s)", id, agent.Id, taskID)
	}
	// Assigning work to a teammate should look immediate: claim it now rather
	// than waiting out the worker's fallback tick, and announce it so the task
	// shows its AI teammate at work straight away.
	WakeAgentTaskWorker()
	publishAgentWorkChanged(ctx, id)
}

// handleTaskCommentForAgent resumes/continues an AI teammate's work on a task
// when a human replies in the task's comment thread. It is the follow-up half of
// the durable engine: a person can answer a blocker ("yes, ship it") or add a
// new instruction ("also update the changelog") right where the work is
// happening, and the agent picks it back up. The agent's OWN status comments
// never reach here (they are workflow-tagged, skipped by the event bus), so
// there is no loop. Inert when AI is disabled.
func handleTaskCommentForAgent(ctx context.Context, eventType string, data map[string]interface{}) {
	if eventType != "task.comment.created" {
		return
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	taskID, _ := data["task_id"].(string)
	authorID, _ := data["author_id"].(string)
	body, _ := data["body"].(string)
	taskID = strings.TrimSpace(taskID)
	body = strings.TrimSpace(body)
	if taskID == "" || body == "" {
		return
	}

	// Continue every AI teammate with a durable job on this task — resume a
	// paused one, or enqueue a follow-up after a finished/failed one — via the
	// unified engine. It finds task-assignment AND code_pr-on-task jobs (a code
	// PR opened from a task has source_id "task:<id>"), so a follow-up comment
	// continues coding work too, not just plain task-assignment work.
	ContinueAgentWork(ctx, taskID, authorID, body)
}

// synthFollowupPrompt builds the run input for a follow-up left after the agent
// already finished a piece of work — a task comment, a thread reply, or a review
// on a PR it opened. Kept surface-agnostic (it drives task, chat, AND code-PR
// follow-ups through the one continuation engine): it states the follow-up and
// asks the agent to continue, then reply concisely. The concrete reply channel
// (task/thread comment, or a fix pushed to the PR) is handled by the job's
// surface + executor, not this prompt.
func synthFollowupPrompt(body string) string {
	return "A teammate followed up on work you previously did:\n\"\"\"\n" +
		promptText(body) + "\n\"\"\"\n\n" +
		"Use your tools as needed to do what they're asking, taking their message into account, then reply concisely with what you did."
}

// synthAssignmentPrompt builds the run input for an assigned task: the task's
// salient fields plus its uuid/project so the agent can act on it with its
// tools, and an instruction to do the work and summarize. The summary becomes
// the status comment posted back to the task.
func synthAssignmentPrompt(data map[string]interface{}) string {
	name, _ := data["name"].(string)
	desc, _ := data["description"].(string)
	taskID, _ := data["task_id"].(string)
	projectID, _ := data["project_id"].(string)
	assignedBy, _ := data["assigned_by_name"].(string)

	var b strings.Builder
	b.WriteString("You have been assigned a task to work on")
	if strings.TrimSpace(assignedBy) != "" {
		b.WriteString(" by " + strings.TrimSpace(assignedBy))
	}
	b.WriteString(".\n\n")
	if strings.TrimSpace(name) != "" {
		b.WriteString("Task: " + strings.TrimSpace(name) + "\n")
	}
	if strings.TrimSpace(desc) != "" {
		b.WriteString("Description:\n\"\"\"\n" + strings.TrimSpace(helpers.HTMLToPlainText(desc)) + "\n\"\"\"\n")
	}
	if refs := appLinkRefs(desc); len(refs) > 0 {
		b.WriteString("The description links to these in OneCamp. Read them with your tools before asking anyone about them:\n")
		for _, r := range refs {
			b.WriteString("- " + r + "\n")
		}
	}
	if strings.TrimSpace(taskID) != "" {
		b.WriteString(fmt.Sprintf("task_uuid: %s\n", strings.TrimSpace(taskID)))
	}
	if strings.TrimSpace(projectID) != "" {
		b.WriteString(fmt.Sprintf("project_uuid: %s\n", strings.TrimSpace(projectID)))
	}
	// "Check the thing itself" is from the demo: a teammate wrote "added them
	// under Decisions", the doc had no such steps, and the agent took the
	// message's word, answering "No new action is required" without opening
	// the doc or saying why.
	b.WriteString("\nUse your tools as needed to actually do the work this task asks for. " +
		"Before deciding something is already done, check the thing itself (read the doc, list the task), not only what someone said about it. " +
		"When you are done, write a concise summary of what you did, or what you found and where if nothing needed doing, and anything you could not complete or that needs a human. " +
		"Your summary will be posted as a comment on the task so the team can see your progress.")
	return b.String()
}

// assignmentTaskUUID extracts the task uuid the job targets (its source_id).
func assignmentTaskUUID(t *model.AgentTask) (uuid.UUID, error) {
	return uuid.Parse(strings.TrimSpace(t.SourceId))
}
