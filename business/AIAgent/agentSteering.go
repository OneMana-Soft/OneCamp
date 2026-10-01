package business

// Mid-run steering — delivering a human's instruction to an agent that is
// already working.
//
// A reply that landed while an agent was running used to go nowhere: the
// continuation path recognised the job as in-flight, suppressed a duplicate
// launch, and dropped the message. So "use the other repo" or "don't touch prod"
// reached the agent only after it had finished doing it the wrong way, and the
// only remedy was to stop the run and lose everything it had done. This is the
// thin worker-side glue over the durable inbox (migration 135); the runner folds
// the messages in between steps.

import (
	"context"
	"strings"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// steerAgentWork hands a human instruction to a job that is queued or already
// running, returning whether it was accepted.
//
// A queued job is included on purpose: a reply that lands in the gap between
// enqueue and claim would otherwise be lost, and the worker drains the inbox
// before its first model call, so it is delivered either way.
//
// TRUST BOUNDARY. Who may steer is decided by the SURFACE: this is only ever
// called for a message somebody was already allowed to post in the thread, task
// or chat the job is working in — the same permission that lets them @mention the
// agent in the first place, and the same one the existing resume-on-reply path
// has always used. Steering therefore widens nobody's reach: the run still
// executes as the agent's OWNER with every governance check unchanged (tool
// allow-list, channel/project scope, autonomy mode, the destructive-action
// approval backstop, per-tool permission re-checks inside each executor). What it
// adds is attribution — the instruction is recorded with its author and shown to
// the model as coming from a named person — so an instruction can never be
// mistaken for the agent's own idea, and the transcript shows who redirected it.
func steerAgentWork(ctx context.Context, taskID uuid.UUID, by string, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	var author *uuid.UUID
	if id, err := uuid.Parse(strings.TrimSpace(by)); err == nil {
		author = &id
	}
	ok, err := model.AppendAgentTaskSteering(ctx, taskID, author, attributeSteering(ctx, author, text))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentSteering: could not queue a mid-run instruction (task=%s): %v", taskID, err)
		return false
	}
	if ok {
		helpers.LogInfoWithContext(ctx, "agentSteering: queued a mid-run instruction (task=%s by=%s)", taskID, by)
	}
	return ok
}

// drainAgentTaskSteering takes and clears the instructions waiting for a leased
// job, flattened to their text for the runner (which is deliberately
// storage-agnostic). Errors degrade to "nothing waiting": a DB blip must slow a
// run down, not break it.
func drainAgentTaskSteering(ctx context.Context, taskID, lease uuid.UUID) []string {
	msgs, err := model.DrainAgentTaskSteering(ctx, taskID, lease)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if text := strings.TrimSpace(m.Text); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// attributeSteering prefixes an instruction with WHO sent it.
//
// Two reasons, both about not misleading anyone. For the model: an unattributed
// line in the conversation reads like the agent's own reasoning, and it needs to
// treat this as a person's instruction. For people: the transcript and the
// agent's acknowledgement then show who redirected the work, which matters when a
// run acts on shared data and several teammates are in the thread.
//
// Best-effort by design — an unresolvable author degrades to the plain text
// rather than dropping the instruction, since delivering it matters more than
// naming it.
func attributeSteering(ctx context.Context, author *uuid.UUID, text string) string {
	text = strings.TrimSpace(text)
	if author == nil || *author == uuid.Nil || text == "" {
		return text
	}
	name := steeringAuthorName(ctx, *author)
	if name == "" {
		return text
	}
	return name + ": " + text
}

// steeringAuthorName resolves a display name for the person steering, preferring
// what they chose to be called. Empty when it can't be resolved (a deleted user,
// a lookup failure) so the caller can fall back cleanly.
func steeringAuthorName(ctx context.Context, author uuid.UUID) string {
	u, err := userBusiness.GetUserByUUID(ctx, author)
	if err != nil || u == nil {
		return ""
	}
	for _, candidate := range []*string{u.DisplayName, u.Username} {
		if candidate != nil {
			if name := strings.Join(strings.Fields(*candidate), " "); name != "" {
				return name
			}
		}
	}
	return ""
}
