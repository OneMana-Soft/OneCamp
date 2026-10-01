package business

// "What is an AI teammate doing HERE?" — live agent work scoped to one surface
// entity (a channel post/thread, a chat message, or a project task).
//
// Why this exists: a person watching an agent work in a thread had to leave that
// thread — open OneCamp AI, then an overflow menu, then a dialog — to stop it or
// even see its state. The work is visible in one place and controllable in
// another. This read lets the surface itself show "working… / stop", which is
// where anybody would look first.
//
// PERMISSIONS. Two different questions, answered separately, because conflating
// them either leaks or over-restricts:
//
//	VIEW — may this person know an agent is working here? Answered by the
//	  SURFACE's own visibility query (channel membership for a thread, project
//	  membership for a task), so this file invents no new permission model and
//	  can never drift from what the surface itself allows. A caller who is
//	  already involved in the job (owner / requester / run-as / admin) always
//	  sees it, since it is their own work.
//	ACT  — may this person stop it? The single canStopAgentWork rule, evaluated
//	  per job and re-checked server-side on the stop call itself. The read only
//	  reports the answer (CanStop) so the UI can offer the control exactly where
//	  it will work, never as a button that 403s.
//
// A surface whose visibility this package cannot verify (a group chat or DM,
// which business/AIAgent deliberately keeps no dependency on) falls back to
// involved-parties-only. That is not a gap in practice: a DM with an agent is
// between that person and the agent.

import (
	"context"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// entityWorkMaxItems caps one entity's live work. A thread with more than a
// handful of agents working at once is pathological; the cap keeps the read cheap.
const entityWorkMaxItems = 10

// ActiveWorkForEntity returns the open durable jobs happening on one surface
// entity that the caller is allowed to see, newest movement first.
//
// entityID is whatever the surface knows: a channel post uuid, a chat message
// uuid, or a task uuid. It never errors on "nothing here" — an empty list is the
// normal case, and the caller (a strip in a thread) renders nothing.
func ActiveWorkForEntity(ctx context.Context, actor Actor, entityID string) ([]ActiveWorkItem, error) {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return nil, nil
	}
	tasks, err := agentDomain.ListActiveTasksForEntity(ctx, entityID, candidateSourceIDs(entityID), entityWorkMaxItems)
	if err != nil {
		return nil, err
	}
	out := make([]ActiveWorkItem, 0, len(tasks))
	requesters := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		principals := activeTaskPrincipals(t)
		canStop := canStopAgentWork(actor, principals)
		if !canStop && !canSeeWorkSurface(ctx, actor, t) {
			continue // not their work, and they can't see where it's happening
		}
		item := toActiveWorkItem(t)
		item.CanStop = canStop
		out = append(out, item)
		// Appended in lockstep with out, not derived afterwards — this loop
		// filters, so the two slices would not line up otherwise.
		requesters = append(requesters, requesterUUID(t))
	}
	attachRequesters(ctx, out, requesters)
	return out, nil
}

// canSeeWorkSurface reports whether the actor can see the SURFACE a job is
// working on, by asking that surface's own visibility query. Conservative by
// construction: anything it cannot verify is a "no", so a new surface kind is
// invisible to bystanders until it is taught here rather than being exposed by
// default.
func canSeeWorkSurface(ctx context.Context, actor Actor, t *model.AgentActiveTask) bool {
	if t == nil || strings.TrimSpace(actor.DgraphUID) == "" {
		return false
	}
	surface := DecodeSurface(t.Surface)
	switch surface.Kind {
	case SurfaceChannelPost:
		// Channel membership is what grants sight of the thread the agent is
		// posting in; the query counts the caller among the channel's members.
		channelID, err := uuid.Parse(strings.TrimSpace(surface.ChannelID))
		if err != nil {
			return false
		}
		info, cerr := channelBusiness.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelID, actor.DgraphUID, "")
		if cerr != nil || info == nil {
			return false
		}
		return info.IsMember > 0
	case SurfaceTask:
		// Project membership is what grants sight of a task; the task's own
		// user-scoped query reports it for the caller.
		taskID := workEntityID(t)
		if taskID == "" {
			return false
		}
		info, terr := taskBusiness.GetDgraphTaskInfo(ctx, taskID, actor.DgraphUID)
		if terr != nil || info == nil || info.Project == nil {
			return false
		}
		return info.Project.IsProjectMember > 0
	default:
		// Group chat / DM: this package holds no chat dependency by design, so
		// visibility can't be proven here. Involved parties already matched above.
		return false
	}
}

// workEntityID resolves the surface entity a job is attached to — the id the
// FRONTEND knows it by (a post uuid, a chat message uuid, or a task uuid).
//
// Generic across every way a job can be routed: the surface descriptor is
// authoritative when present, and otherwise source_id is used, stripping the
// "post:" / "msg:" / "task:" prefixes a coding job carries (mirroring
// codepr.SourceID) so a code PR opened from a thread still resolves to that
// thread. Empty when the job has nothing a surface could match.
func workEntityID(t *model.AgentActiveTask) string {
	if t == nil {
		return ""
	}
	surface := DecodeSurface(t.Surface)
	if id := strings.TrimSpace(surface.PostID); id != "" {
		return id
	}
	if id := strings.TrimSpace(surface.MessageID); id != "" {
		return id
	}
	id := strings.TrimSpace(t.SourceId)
	for _, prefix := range []string{"post:", "msg:", "task:"} {
		id = strings.TrimPrefix(id, prefix)
	}
	return id
}
