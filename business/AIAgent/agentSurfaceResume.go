package business

// Resume-on-human-follow-up for durable runs on NON-task surfaces (async-mentions
// spec Task 7). handleTaskCommentForAgent already resumes a paused durable job
// when a human replies in a project task's comment thread; this generalizes the
// SAME behaviour to a channel-post thread, a group-chat thread, and a 1:1 DM
// thread, so a durable run that paused (awaiting_input on a needs_human blocker
// or a budget cap) picks its work back up the moment a teammate answers in the
// same thread — no re-@mention needed. Kept surface-agnostic: it keys off the
// job's Surface descriptor (source_type = SurfaceKind, source_id = the parent
// post/message), so a new surface reuses this untouched.

import (
	"context"

	"github.com/google/uuid"
)

// ResumeDurableRunsForSurface resumes every paused (awaiting_input) durable
// agent job on one reply surface when a human posts a follow-up in the thread.
// It is the non-task analog of handleTaskCommentForAgent: a person answering a
// blocked run's question — or just nudging it — in the SAME thread resumes the
// durable job in place, without re-@mentioning the agent. The follow-up is
// appended to the durable conversation so the resumed run continues with full
// context.
//
// Returns the set of agent ids whose jobs were resumed, so the caller can skip
// launching a duplicate (synchronous or fresh-durable) run for those agents.
// authorID guards against an agent's OWN status comment resuming its own job
// (the loop guard). Best-effort: a lookup/resume error for one job never blocks
// the others, and an empty result simply means "nothing was paused here".
func ResumeDurableRunsForSurface(ctx context.Context, kind SurfaceKind, sourceID, authorID, followup string) map[uuid.UUID]bool {
	if !validSurfaceKind(kind) {
		return map[uuid.UUID]bool{}
	}
	// Delegate to the unified engine, which finds jobs by the surface entity id
	// across ALL source types (durable mention/thread + code_pr) and both
	// resumes paused jobs AND enqueues a follow-up after a finished/failed one —
	// so a reply after the agent already finished continues its work instead of
	// being dropped (the gap this used to have: awaiting_input only).
	return ContinueAgentWork(ctx, sourceID, authorID, followup)
}

// ResumeDurableChatRuns continues durable jobs on a chat message thread (group
// OR 1:1 DM) when a human follows up. The unified engine keys on the message id
// regardless of whether the job was enqueued as group_chat or dm, so a single
// call covers both. Returns the set of handled agent ids.
func ResumeDurableChatRuns(ctx context.Context, messageID, authorID, followup string) map[uuid.UUID]bool {
	return ContinueAgentWork(ctx, messageID, authorID, followup)
}
