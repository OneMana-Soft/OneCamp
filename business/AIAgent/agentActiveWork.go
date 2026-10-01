package business

// Active agent work — the live "what are my AI teammates doing right now, and
// where are they blocked on me" view. Durable jobs (ai_agent_tasks) that are
// queued / running / awaiting_input are otherwise only visible in the one thread
// they posted a status comment in; this surfaces them together as a render-ready
// feed, scoped to what the caller may see (admins: whole workspace; members:
// agents they own — the SAME scoping as the activity feed and reliability
// rollups). No new store: it reads the durable-queue record and derives a
// generic, surface-agnostic label from the job's Surface descriptor.

import (
	"context"
	"strings"
	"time"

	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

const activeWorkMaxItems = 100

// ActiveWorkState is the coarse, user-facing lifecycle bucket of a durable job.
// It collapses the internal queue states into the three things a person cares
// about: is it lined up, actively working, or waiting on someone.
type ActiveWorkState string

const (
	ActiveWorkQueued  ActiveWorkState = "queued"  // lined up, not started yet
	ActiveWorkWorking ActiveWorkState = "working" // actively running now
	ActiveWorkBlocked ActiveWorkState = "blocked" // paused, waiting on a human / a reset
	// ActiveWorkStopping: a person asked it to stop and the worker running it is
	// wrapping up (cancellation is cooperative, so this is a real, brief state —
	// showing "working" here would look like the Stop button did nothing).
	ActiveWorkStopping ActiveWorkState = "stopping"
	// ActiveWorkStopped: terminal, stopped by a person. Open feeds never carry
	// this (they only list open jobs); it is what the stop API reports back.
	ActiveWorkStopped ActiveWorkState = "stopped"
)

// ActiveWorkItem is one live durable job, render-ready. It is deliberately
// surface-agnostic: the FE renders the same card whatever the reply surface is,
// using a generic human label + the coarse state.
type ActiveWorkItem struct {
	TaskID         string          `json:"task_id"`
	AgentID        string          `json:"agent_id"`
	AgentName      string          `json:"agent_name"`
	AgentAvatarKey string          `json:"agent_avatar_key,omitempty"`
	State          ActiveWorkState `json:"state"`
	// Where reads like "in a channel thread", "in a direct message", "on a task"
	// — a generic, layering-safe description derived from the Surface kind (no
	// cross-package name lookups, so business/AIAgent stays dependency-light).
	Where string `json:"where"`
	// Note is the blocker/pause reason for a blocked job (from the run's honest
	// stop reason), or "" otherwise — the "what it's waiting on you for" line.
	Note string `json:"note,omitempty"`
	// Options are the answers the agent offered with its question, when it
	// offered any. Split out server-side rather than left inside Note for the
	// FE to pick apart: the choices a person taps must be the same list the
	// resume matches their reply against, and prose parsed in the browser is
	// not that list.
	Options []string `json:"options,omitempty"`
	// StartedAt is when the job was first enqueued; UpdatedAt when it last moved
	// (so the FE can show "blocked 2h ago").
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
	// RequestedBy is the display name of the PERSON this work is attributed to —
	// the one who asked. Empty when nobody can be resolved (a scheduled routine
	// has no requester, and a deleted user should read as absent rather than as a
	// raw uuid).
	//
	// Worth having even without delegation: in a shared channel where three people
	// ping the same agent, "working…" says nothing about whose request is in
	// flight. With delegation it is what makes a chain accountable — the run is
	// attributed to the originating human rather than the agent that relayed it,
	// so this always names someone who could have done it themselves.
	//
	// Not a new disclosure: a caller only receives an item they are already
	// authorised to see (surface visibility or involvement), and anyone who can
	// see the thread can already see who posted in it.
	RequestedBy string `json:"requested_by,omitempty"`
	// CanStop reports whether the CALLER may stop this job, so the UI can offer
	// the control only where it will work. Authorization is decided server-side
	// (agent owner / requester / run-as user / admin) rather than inferred from
	// visibility, since a job can post somewhere the viewer can't see.
	CanStop bool `json:"can_stop"`
	// EntityID is the surface entity this work is attached to — the channel post,
	// chat message, or task uuid the FRONTEND already knows it by. It is what lets
	// the place where the work is happening show and stop it, instead of sending
	// people to a separate panel. Empty for work with no addressable surface.
	EntityID string `json:"entity_id,omitempty"`
	// Surface is the coarse kind of that entity ("channel_post", "task", …), so a
	// caller can render the right affordance without parsing ids.
	Surface string `json:"surface,omitempty"`
}

// ActiveWork returns the open durable jobs the actor may see, newest-movement
// first. Admins see the whole workspace; members see only agents they own. A
// blocked job carries a short, honest reason so the person knows it is waiting
// on them, not stuck silently.
func ActiveWork(ctx context.Context, actor Actor, limit int) ([]ActiveWorkItem, error) {
	if limit <= 0 || limit > activeWorkMaxItems {
		limit = activeWorkMaxItems
	}
	var createdBy *uuid.UUID
	if !actor.IsAdmin {
		id := actor.UserID
		createdBy = &id
	}
	tasks, err := agentDomain.ListActiveTasks(ctx, createdBy, limit)
	if err != nil {
		return nil, err
	}
	return authorizedWorkItems(ctx, actor, tasks), nil
}

// MyActiveWork returns the open durable jobs the actor is personally involved
// in — the ones they triggered or that run as them — across ANY agent, not just
// agents they own. This is the member-facing "AI teammates working for me /
// waiting on me" view (distinct from ActiveWork, which is owner-scoped). Newest-
// movement first; a blocked job carries its honest "waiting on you" reason.
func MyActiveWork(ctx context.Context, actor Actor, limit int) ([]ActiveWorkItem, error) {
	if limit <= 0 || limit > activeWorkMaxItems {
		limit = activeWorkMaxItems
	}
	tasks, err := agentDomain.ListActiveTasksForActor(ctx, actor.UserID, limit)
	if err != nil {
		return nil, err
	}
	return authorizedWorkItems(ctx, actor, tasks), nil
}

// authorizedWorkItems renders rows for a feed, stamping each one with whether
// THIS caller may stop it. Both feeds are already scoped to work the actor is
// entitled to see (agents they own / jobs they're party to), so nothing is
// filtered here — but the stop flag is derived from the same single rule the stop
// endpoint enforces rather than assumed from how the feed was scoped, so a future
// widening of a feed can't silently offer a control that would be refused.
func authorizedWorkItems(ctx context.Context, actor Actor, tasks []*model.AgentActiveTask) []ActiveWorkItem {
	out := make([]ActiveWorkItem, 0, len(tasks))
	requesters := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		item := toActiveWorkItem(t)
		item.CanStop = canStopAgentWork(actor, activeTaskPrincipals(t))
		out = append(out, item)
		requesters = append(requesters, requesterUUID(t))
	}
	attachRequesters(ctx, out, requesters)
	return out
}

// toActiveWorkItem maps a joined durable-job row into a render-ready item.
func toActiveWorkItem(t *model.AgentActiveTask) ActiveWorkItem {
	surface := DecodeSurface(t.Surface)
	item := ActiveWorkItem{
		TaskID:    t.Id.String(),
		AgentID:   t.AgentId.String(),
		AgentName: strings.TrimSpace(t.AgentName),
		State:     activeWorkState(t.State),
		Where:     surfaceWhereLabel(surface),
		StartedAt: t.CreatedAt.Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
		EntityID:  workEntityID(t),
		Surface:   string(surface.Kind),
	}
	// A stop already asked for reads as "stopping", not "working": cancellation
	// is cooperative (the worker wraps up its run first), and showing the old
	// state would make the Stop control look like it did nothing.
	if t.StopRequested {
		item.State = ActiveWorkStopping
	}
	if t.AgentAvatarKey != nil {
		item.AgentAvatarKey = strings.TrimSpace(*t.AgentAvatarKey)
	}
	// Only a blocked job surfaces a reason: it is the "waiting on you" signal.
	if item.State == ActiveWorkBlocked && t.LastError != nil {
		item.Note, item.Options = blockerNote(*t.LastError)
	}
	return item
}

// activeWorkState collapses an internal queue state into the coarse user bucket.
func activeWorkState(state string) ActiveWorkState {
	switch state {
	case model.TaskRunning:
		return ActiveWorkWorking
	case model.TaskAwaiting:
		return ActiveWorkBlocked
	case model.TaskCancelled:
		return ActiveWorkStopped
	default: // TaskQueued (and any unknown open state) reads as queued
		return ActiveWorkQueued
	}
}

// surfaceWhereLabel renders a generic, layering-safe "where the work is
// happening" phrase from the reply surface kind. It intentionally avoids
// resolving channel/task NAMES (which would pull this package into chat/task
// deps and require extra reads); the in-thread status comment is the click
// target for the specifics.
func surfaceWhereLabel(s Surface) string {
	switch s.Kind {
	case SurfaceChannelPost:
		return "in a channel thread"
	case SurfaceGroupChat:
		return "in a group chat"
	case SurfaceDM:
		return "in a direct message"
	case SurfaceTask:
		return "on a task"
	default:
		return "on a request"
	}
}

// blockerNote turns a stored last_error into a short, human "waiting on you"
// line plus the answers the agent offered, if any.
//
// The durable worker parks a needs_human blocker with "blocked: <rendered
// question>" and a budget pause with a budget message; we strip the internal
// prefix and cap the length so the card stays skimmable.
//
// THE OPTIONS COME OUT BEFORE THE WHITESPACE IS COLLAPSED, which is the whole
// reason this returns two values. Fields() flattens the rendered list into one
// run-on line, so a question with choices used to reach the card as
// "Which environment? - staging - production" and a person had to read an
// enumeration out of a sentence. Pure.
func blockerNote(raw string) (string, []string) {
	body := strings.TrimPrefix(strings.TrimSpace(raw), "blocked: ")
	elic := ParseRendered(body)

	note := strings.Join(strings.Fields(elic.Question), " ")
	const max = 160
	if len([]rune(note)) > max {
		note = strings.TrimSpace(string([]rune(note)[:max])) + "…"
	}
	return note, elic.Options
}

// attachRequesters fills RequestedBy across a page of items in ONE query.
//
// Deliberately batched rather than resolved inside toActiveWorkItem: a per-row
// lookup would turn a 20-item list into 20 round trips, and the same person
// usually triggered several of them. Best-effort — a resolution failure leaves the
// names empty rather than failing the list, because "who asked" is context, not
// the payload, and an agent-work list that 500s is worse than one without names.
//
// Generic over any slice of items so every active-work view (mine, per-entity,
// admin) shares one implementation instead of each remembering to do it.
// requesterUUIDs is parallel to items and supplied by the caller as it appends,
// NOT derived from the task slice. Every active-work feed filters rows (a
// bystander's per-entity view drops work they can't see), so tasks and items are
// not index-aligned and deriving the mapping here would silently attribute a row
// to the wrong person. Making the caller carry it makes that mistake impossible.
func attachRequesters(ctx context.Context, items []ActiveWorkItem, requesterUUIDs []string) {
	if len(items) == 0 || len(items) != len(requesterUUIDs) {
		return
	}
	seen := make(map[string]struct{}, len(requesterUUIDs))
	ids := make([]string, 0, len(requesterUUIDs))
	for _, id := range requesterUUIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	displays, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentActiveWork: resolve requesters failed: %v", err)
		return
	}
	for i, id := range requesterUUIDs {
		d, ok := displays[id]
		if !ok {
			continue
		}
		// Full name when known, handle otherwise: the row is dense, so one name
		// is shown rather than "Full Name (@handle)".
		if n := strings.TrimSpace(d.FullName); n != "" {
			items[i].RequestedBy = n
		} else {
			items[i].RequestedBy = strings.TrimSpace(d.Name)
		}
	}
}

// requesterUUID is the person a job is attributed to, or "" when there is none
// (a scheduled routine has no requester).
func requesterUUID(t *model.AgentActiveTask) string {
	if t == nil || t.TriggeredBy == nil {
		return ""
	}
	return t.TriggeredBy.String()
}
