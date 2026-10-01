package business

// Live agent-work events — the reason the in-thread "working… / Stop" strip does
// not poll.
//
// Without these, every client watching a thread or a task had to ask the API on a
// timer to notice that an agent started, was asked to stop, or finished. That is
// wasteful everywhere and actively bad on a phone: OneCamp installs as a PWA, so
// a timer means waking a device to re-read a list that changes a handful of times
// per run. A durable job moves state a few times in its life, so pushing those
// few moments is strictly cheaper than every client asking.
//
// Where the event goes, and why:
//   - the SURFACE's message topic (the channel of the thread, the project of the
//     task, the chat grouping) — everyone already subscribed to that surface gets
//     it with no new subscription, because the MQTT config hands each client its
//     channel/project/DM topics at connect time;
//   - the ACTIVITY topic of each person involved in the job (the agent's owner,
//     whoever asked, whoever it runs as) — so their "AI teammates" view updates
//     wherever they happen to be.
//
// Payload discipline: the event carries only what the strip already shows to
// anyone who can see that surface (which agent, what state). It never carries
// whether the RECEIVER may stop the job — that is per-person and re-checked
// server-side — so the same message is safe to broadcast to a shared topic.
//
// Best-effort throughout: a broker or lookup failure must never affect the run.

import (
	"context"
	"strings"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// publishAgentWorkChanged announces a durable job's current state to the surface
// it runs on and the people involved in it.
//
// It re-reads the job so the event always reflects committed state rather than
// what the caller believed a moment ago — a job can be reclaimed or settled by
// another worker between a decision and its transition, and publishing a stale
// state would make a client show something the store disagrees with.
func publishAgentWorkChanged(ctx context.Context, taskID uuid.UUID) {
	if taskID == uuid.Nil {
		return
	}
	// Detached: a stop or a lost lease cancels the run context, and those are
	// exactly the transitions worth announcing.
	ctx = context.WithoutCancel(ctx)
	t, err := model.GetAgentTaskByID(ctx, taskID)
	if err != nil || t == nil {
		return
	}
	agentName := ""
	var agentOwner uuid.UUID
	if a, aerr := model.GetAgentByID(ctx, t.AgentId); aerr == nil && a != nil {
		agentName = strings.TrimSpace(a.Name)
		agentOwner = a.CreatedBy
	}

	state := activeWorkState(t.State)
	if t.CancelRequestedAt != nil && isOpenTaskState(t.State) {
		state = ActiveWorkStopping
	}
	payload := &mqttStruct.MqttAgentWork{
		EntityID:  agentTaskEntityID(t),
		TaskID:    t.Id.String(),
		AgentId:   t.AgentId.String(),
		AgentName: agentName,
		State:     string(state),
		Open:      isOpenTaskState(t.State),
		UpdatedAt: t.UpdatedAt.UTC().Format(timeRFC3339),
	}
	mqttBusiness.PublishToTopics(agentWorkTopics(ctx, t, agentOwner), mqttStruct.MESSAGE_AI_AGENT_WORK, payload)
}

// timeRFC3339 keeps the wire format identical to the REST payloads the same
// clients consume, so a client can compare/merge timestamps without parsing two
// formats.
const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// isOpenTaskState reports whether a job is still in flight (so a client should
// keep showing it) rather than terminal (drop it, no follow-up request needed).
func isOpenTaskState(state string) bool {
	for _, open := range model.OpenTaskStates {
		if state == open {
			return true
		}
	}
	return false
}

// agentWorkTopics resolves everywhere this event should land: the surface's own
// message topic plus the activity topic of each person involved. Blank/duplicate
// topics are dropped by the publisher, so every branch may fail quietly.
func agentWorkTopics(ctx context.Context, t *model.AgentTask, agentOwner uuid.UUID) []string {
	var topics []string

	surface := DecodeSurface(t.Surface)
	switch surface.Kind {
	case SurfaceChannelPost:
		if surface.ChannelID != "" {
			topics = append(topics, helpers.GetMqttTopicForChannelMessage(surface.ChannelID))
		}
	case SurfaceGroupChat, SurfaceDM:
		if surface.GroupID != "" {
			topics = append(topics, helpers.GetMqttTopicForDmMessage(surface.GroupID))
		}
	case SurfaceTask:
		// A task's live topic is its PROJECT's (that is where task comments and
		// GitHub sync already publish), and the job row only knows the task — so
		// resolve the project once, best-effort. Skipping it costs the project's
		// other members a live update, never correctness.
		if projectID := taskProjectID(ctx, agentTaskEntityID(t)); projectID != "" {
			topics = append(topics, helpers.GetMqttTopicForProjectMessage(projectID))
		}
	}

	for _, person := range []*uuid.UUID{t.TriggeredBy, t.RunAsUserId} {
		if person != nil && *person != uuid.Nil {
			topics = append(topics, helpers.GetMqttTopicForUserActivity(person.String()))
		}
	}
	if agentOwner != uuid.Nil {
		topics = append(topics, helpers.GetMqttTopicForUserActivity(agentOwner.String()))
	}
	return topics
}

// taskProjectID resolves the project a task belongs to. Read with no acting user
// (this is an internal fan-out decision, not a permission check — every recipient
// is already scoped by the project topic they subscribe to). Empty on any problem.
func taskProjectID(ctx context.Context, taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ""
	}
	// Event feed, no requesting user at all.
	info, err := taskBusiness.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), taskID, "")
	if err != nil || info == nil || info.Project == nil {
		return ""
	}
	return strings.TrimSpace(info.Project.Uuid)
}

// agentTaskEntityID is the surface-entity resolver for a full job row — the same
// rule workEntityID applies to a joined active-work row (surface descriptor first,
// then source_id with the code_pr prefixes stripped), kept in one shape so the
// event and the REST payload always agree on what a client should match on.
func agentTaskEntityID(t *model.AgentTask) string {
	if t == nil {
		return ""
	}
	return workEntityID(&model.AgentActiveTask{Surface: t.Surface, SourceId: t.SourceId})
}
