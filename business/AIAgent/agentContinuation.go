package business

// Unified agent-work continuation.
//
// A human follow-up on a surface an AI teammate already worked (a channel-post
// thread, a group/DM chat thread, a project task, or the OneCamp thread behind
// a code-PR job) should CONTINUE that teammate's work — whether the prior job is
// paused, finished, or failed — without a re-@mention. Previously this logic was
// split across three places with different behavior:
//   - the task path resumed awaiting_input AND enqueued a follow-up after
//     done/failed (agentAssignment.handleTaskCommentForAgent),
//   - the channel/chat surface path resumed ONLY awaiting_input
//     (agentSurfaceResume.ResumeDurableRunsForSurface),
//   - and code_pr jobs (a distinct source_type + prefixed source_id) were
//     invisible to BOTH lookups.
//
// ContinueAgentWork is the single, surface-agnostic engine all three now use. It
// finds every durable job for a surface entity across ALL source types (durable
// mention/thread jobs, task-assignment jobs, and code_pr jobs — via
// ListAgentTasksForEntity), then per agent: resumes a paused job in place, or
// enqueues ONE fresh follow-up job after a finished/failed one, reusing the
// durable engine (idempotent against a comment burst via the open-job unique
// index). The loop guard drops the agent's own status comment.
//
// WHOSE WORK IT IS. A job runs for the person who asked for it (triggered_by),
// reaching only what they and the agent's sponsor both can. Folding someone
// else's message into it — steering a running job, answering a paused one —
// would carry out that person's instruction with the asker's reach, so only the
// asker and the sponsor may continue a job in place. Anyone else is told so and
// can ask the agent themselves; a follow-up after the job has finished is their
// own new job, for them. A message nobody identified wrote continues nothing:
// feedback from outside OneCamp, on a pull request a job opened, goes to the
// owner of the account it would push with instead (ProposePullRequestFeedback).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	codepr "github.com/akashc777/OneCamp/business/CodePR"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// candidateSourceIDs returns the source_id values that denote a surface entity
// across the job kinds that key on source_id: the bare id (durable mention/
// thread jobs and task-assignment jobs) and the code_pr prefixed forms (mirrors
// codepr.SourceID — kept here so the model layer stays free of that convention).
// The surface descriptor (post_id/message_id) is matched separately by
// ListAgentTasksForEntity, so channel/chat durable jobs are found either way.
func candidateSourceIDs(entityID string) []string {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return nil
	}
	return []string{
		entityID,
		"post:" + entityID,
		"msg:" + entityID,
		"task:" + entityID,
	}
}

// ContinueAgentWork continues every AI teammate that has a durable job on one
// surface entity when a human follows up there. entityID is the id of the thing
// replied on (a post uuid, chat message uuid, or task uuid); authorID is the
// follow-up author (for the loop guard); followup is their message.
//
// A job that is already in flight is STEERED rather than left alone: the message
// is handed to the running job's inbox and the agent folds it in before its next
// action, so a mid-run correction no longer arrives too late to matter.
//
// Returns the set of agent ids whose work this call HANDLED (resumed, steered,
// or followed up), so a caller can skip launching a duplicate fresh
// (synchronous or durable) run for those agents — a follow-up never both
// continues a durable job AND starts a competing one. Best-effort: an error on
// one job never blocks the others; an empty result means nothing durable exists
// for this surface (the caller handles it as a fresh interaction).
func ContinueAgentWork(ctx context.Context, entityID, authorID, followup string) map[uuid.UUID]bool {
	handled := map[uuid.UUID]bool{}
	entityID = strings.TrimSpace(entityID)
	followup = promptText(followup)
	if entityID == "" || followup == "" {
		return handled
	}

	tasks, err := model.ListAgentTasksForEntity(ctx, entityID, candidateSourceIDs(entityID))
	if err != nil || len(tasks) == 0 {
		return handled
	}

	// Newest-first (the query orders by created_at DESC): the first job seen for
	// an agent is its most recent on this surface, which is the one to continue.
	for _, t := range tasks {
		if t == nil || handled[t.AgentId] {
			continue
		}
		agent, gerr := model.GetAgentByID(ctx, t.AgentId)
		if gerr != nil || agent == nil {
			continue
		}
		// Written by nobody identified: it may not steer, answer or follow up
		// anyone's work, which would carry it out with the reach of whoever the
		// job is for. Handled, so no competing run starts either.
		if strings.TrimSpace(authorID) == "" {
			handled[t.AgentId] = true
			helpers.LogInfoWithContext(ctx, "ContinueAgentWork: a follow-up with no author was not given to job %s", t.Id)
			continue
		}
		// Loop guard: an agent's own status/result comment must never drive its
		// own work. Resolve the bot principal once per agent.
		if authorID != "" {
			if bot, berr := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey)); berr == nil && bot != nil {
				if authorID == bot.UUID || authorID == bot.DgraphUID {
					handled[t.AgentId] = true // suppress a fresh launch too
					continue
				}
			}
		}

		// Steering or resuming in place hands this message to a run that acts for
		// the job's asker; only they or the sponsor may do that (see above).
		inPlace := t.State == model.TaskAwaiting || t.State == model.TaskQueued || t.State == model.TaskRunning
		if inPlace && !mayContinueInPlace(t, agent, authorID) {
			handled[t.AgentId] = true // their message must not start a competing run either
			if ClassifyMentionIntent(followup, agent) != IntentGreeting {
				tellOnJobSurface(ctx, agent, t, notYourJobNote(ctx, agent, t))
			}
			helpers.LogInfoWithContext(ctx, "ContinueAgentWork: %s may not steer job %s, which is for someone else", authorID, t.Id)
			continue
		}

		switch t.State {
		case model.TaskAwaiting:
			// Paused on a needs_human/budget blocker: resume in place with the
			// follow-up appended to the durable conversation.
			//
			// Resolved against what was actually ASKED before it is appended. If
			// the agent offered choices and the person picked one, the run is
			// told the decision rather than the sentence, and a refusal is told
			// as a refusal. Every other pause resumes with the raw words, which
			// is what it has always done.
			resume := followup
			if t.LastError != nil {
				resume = resolveResumeText(*t.LastError, followup)
			}
			// Added after the job's asker-words trailer, so it carries no mark of
			// its own (the question it may quote is the agent's text).
			resume = withoutSep(resume)
			if ok, rerr := model.ResumeAgentTaskWithFollowup(ctx, t.Id, resume); rerr == nil && ok {
				handled[t.AgentId] = true
				WakeAgentTaskWorker() // a person is waiting on this answer
			}
		case model.TaskQueued, model.TaskRunning:
			// A job for this agent is already in flight on this surface, so we
			// must not enqueue a competing follow-up (the open-job unique index
			// would reject it anyway) or let the caller launch a fresh run — the
			// in-flight job owns this agent's continuation here.
			//
			// But "in flight" is no reason to LOSE the message: hand it to the
			// running job as steering, so the correction reaches the agent before
			// its next action instead of after it finished the work the wrong way.
			// Best-effort — a job that settled in the meantime simply doesn't take
			// it, and the reply is handled as it was before.
			steerAgentWork(ctx, t.Id, authorID, followup)
			handled[t.AgentId] = true
		case model.TaskDone, model.TaskFailed, model.TaskCancelled:
			// New instruction after the agent finished, failed, or was STOPPED by
			// a person: enqueue ONE fresh follow-up, reusing the ORIGINAL job's
			// routing (source_type, source_id, surface, owner, attempt budget) so
			// a code_pr follow-up re-enters the coding orchestrator and a thread
			// follow-up posts back to the same thread. The open-job index makes a
			// comment burst idempotent. A stopped job is included deliberately:
			// the person ended the OLD work, and a new instruction on the same
			// surface should still reach the agent where they're talking to it —
			// as a fresh job, never as a resume of what they just stopped.
			ft := &model.AgentTask{
				AgentId:     agent.Id,
				SourceType:  t.SourceType,
				SourceId:    t.SourceId,
				Prompt:      synthFollowupPrompt(followup),
				RunAsUserId: t.RunAsUserId,
				Surface:     t.Surface,
				MaxAttempts: t.MaxAttempts,
			}
			// The follow-up is its author's words (a coding job's prompt is its
			// instruction, read whole, and needs none).
			if t.SourceType != codepr.TaskSourceType {
				ft.Prompt = withAskerWordsTrailer(ft.Prompt, []string{followup})
			}
			if tb, perr := uuid.Parse(strings.TrimSpace(authorID)); perr == nil {
				// The follow-up is its author's own request, for them.
				ft.TriggeredBy = &tb
				// A coding follow-up pushes with its author's GitHub account, not
				// with the account of whoever asked for the earlier change.
				if t.SourceType == codepr.TaskSourceType {
					ft.RunAsUserId = &tb
				}
			}
			if id, created, eerr := model.EnqueueAgentTask(ctx, ft); eerr == nil {
				// "Keep going" after a job ran out of steps continues its
				// conversation rather than starting over. Only for the people who
				// may continue it: the conversation holds what the earlier run read
				// for its asker, which is not someone else's to pick up.
				if carried := carryOverConversation(t, followup); created && carried != "" && mayContinueInPlace(t, agent, authorID) {
					if _, serr := seedConversation(ctx, id, carried); serr != nil {
						helpers.LogErrorWithContext(ctx, "ContinueAgentWork: carry the conversation into %s failed: %v", id, serr)
					}
				}
				handled[t.AgentId] = true
				WakeAgentTaskWorker()
			}
		}
	}
	return handled
}

// mayContinueInPlace reports whether authorID may add to job t as it stands:
// steer it, answer it, or carry its conversation forward. The asker may, and so
// may the sponsor, whose instruction gains nothing (the job stays bounded by
// the asker's reach). Nobody else may, and nor may a message without an author:
// from outside OneCamp, it is anyone's (a comment on a public repository).
func mayContinueInPlace(t *model.AgentTask, agent *model.AiAgent, authorID string) bool {
	author := strings.TrimSpace(authorID)
	if author == "" {
		return false
	}
	if agent != nil && strings.EqualFold(author, agent.CreatedBy.String()) {
		return true
	}
	return t != nil && t.TriggeredBy != nil && strings.EqualFold(author, t.TriggeredBy.String())
}

// notYourJobNote tells someone why the agent did not take their message: the
// work in this thread is someone else's request. A job its sponsor asked for
// is the sponsor's alone. A job with no recorded asker is nobody's, and only
// the sponsor may add to it (mayContinueInPlace).
func notYourJobNote(ctx context.Context, agent *model.AiAgent, t *model.AgentTask) string {
	sponsor := personName(ctx, agent.CreatedBy.String(), "the person who set me up")
	if t == nil || t.TriggeredBy == nil || *t.TriggeredBy == uuid.Nil {
		return fmt.Sprintf("I don't have a record of who asked for this, so I can only take instructions on it from %s. "+
			"If you need something yourself, ask me separately.", sponsor)
	}
	if *t.TriggeredBy == agent.CreatedBy {
		return fmt.Sprintf("I'm working on this for %s, so I can only take instructions on it from them. "+
			"If you need something yourself, ask me separately.", sponsor)
	}
	asker := personName(ctx, t.TriggeredBy.String(), "the person who asked")
	return fmt.Sprintf("I'm working on this for %s, so I can only take instructions on it from them or from %s. "+
		"If you need something yourself, ask me separately.", asker, sponsor)
}

// tellOnJobSurface posts a one-off note, as the agent, where job t talks to
// people. Best-effort: a note that cannot be posted is logged by the poster.
func tellOnJobSurface(ctx context.Context, agent *model.AiAgent, t *model.AgentTask, text string) {
	surface := DecodeSurface(t.Surface)
	switch surface.Kind {
	case SurfaceTask:
		postAgentTaskStatus(ctx, agent, t, text)
	case SurfaceChannelPost:
		postAgentReply(ctx, agent, resolveAgentBot(ctx, agent), surface.ChannelID, surface.PostID, text)
	default:
		if p := newStatusPoster(ctx, agent, surface); p != nil {
			p.Set(ctx, text)
		}
	}
}

// seedConversation starts a queued job from a saved conversation; a seam.
var seedConversation = model.SeedAgentTaskMessages

// boundedStopErrors are how a job that ran out of room records it (see
// finalizeStopped in agentRunner.go).
var boundedStopErrors = map[string]bool{
	"reached the step limit":          true,
	"reached the per-run token limit": true,
}

// carryOverConversation returns the conversation a follow-up job should start
// from, or "" to start fresh. Only a job that stopped because it ran out of
// steps carries over: its reply says "reply here and I'll keep going", and
// keeping going means knowing what it already read and did (the runner also
// rebuilds its record of earlier calls from it, so a write is never repeated).
// Any other finished job's follow-up is new work and starts clean. Pure.
func carryOverConversation(prior *model.AgentTask, followup string) string {
	if prior == nil || prior.LastError == nil || !boundedStopErrors[strings.TrimSpace(*prior.LastError)] {
		return ""
	}
	var msgs []map[string]interface{}
	if err := json.Unmarshal([]byte(prior.Messages), &msgs); err != nil || len(msgs) == 0 {
		return ""
	}
	msgs = append(msgs, map[string]interface{}{
		"role":    "user",
		"content": "A teammate replied: " + strings.TrimSpace(followup) + "\nContinue the work from where you stopped, taking their reply into account.",
	})
	b, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(b)
}
