package business

// Proactive notification to the person who triggered a durable agent run
// (async-mentions: "tag the human when it's blocked or done"). When a background
// run pauses awaiting a human decision — or finishes — the person who @mentioned
// the agent gets an in-app activity ping (the same MQTT activity + push channel a
// human @mention uses), so they aren't left refreshing a thread. This complements
// the evolving in-thread status comment: the comment is the record, this is the
// nudge to the right individual.
//
// It reuses the SAME low-level primitive as the human mention path
// (activityBusiness.PublishActivityToUser), so priority scoring, MQTT fan-out,
// and the activity feed all behave identically — no parallel notification path.
// Surface-agnostic: it builds the right activity item from the run's Surface, so
// a new surface is a new case here, not a new pipeline. Best-effort and
// fire-and-forget: a delivery miss never blocks the run's state machine.

import (
	"context"
	"strings"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userFCMTokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// notifyTriggerer sends an in-app activity ping to the person who triggered a
// durable run, for a block/finish moment. triggeredBy is their app user uuid
// (empty → no-op, e.g. a scheduled job with no human trigger). text is the
// short, human message (the block reason or the final summary). Best-effort.
//
// The agent's own badged principal is the "from" so the ping reads as coming
// from the teammate. If the triggerer IS the agent's principal (shouldn't
// happen — an agent can't @mention itself into a durable run), it is skipped.
func notifyTriggerer(ctx context.Context, agent *model.AiAgent, surface Surface, triggeredBy, text string) {
	triggeredBy = strings.TrimSpace(triggeredBy)
	text = strings.TrimSpace(text)
	if agent == nil || triggeredBy == "" || text == "" {
		return
	}
	bot := resolveAgentBot(ctx, agent)
	if bot == nil {
		return
	}
	if triggeredBy == bot.UUID || triggeredBy == bot.DgraphUID {
		return // never notify the agent about itself
	}
	item := buildTriggererActivity(agent, bot, surface, text)
	if item == nil {
		return
	}
	activityBusiness.PublishActivityToUser(triggeredBy, item)
}

// buildTriggererActivity constructs the MENTION activity item for a surface,
// mirroring the shapes the human mention path publishes (post-comment mention
// for channel posts; chat mention for group/DM), so the FE renders + deep-links
// it with the existing handler. Returns nil for a surface with no notifiable
// target (e.g. the legacy task surface, which has its own task-comment path).
func buildTriggererActivity(agent *model.AiAgent, bot *userBusiness.BotIdentity, surface Surface, text string) *dgraphModels.UnifiedActivityItem {
	now := time.Now()
	from := &dgraphStruct.DgraphUser{Uuid: bot.UUID, UserName: agent.Name}

	switch surface.Kind {
	case SurfaceChannelPost:
		if surface.PostID == "" {
			return nil
		}
		return &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         now.Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				PostUuid: surface.PostID,
				Post: &dgraphStruct.DgraphPost{
					Uuid:    surface.PostID,
					Channel: &dgraphStruct.DgraphChannel{Uuid: surface.ChannelID},
				},
				Comment: &dgraphStruct.DgraphComment{
					Text:      text,
					CommentBy: from,
					Post: &dgraphStruct.DgraphPost{
						Uuid:    surface.PostID,
						Channel: &dgraphStruct.DgraphChannel{Uuid: surface.ChannelID},
					},
					CreatedAt: &now,
				},
				CreatedAt: &now,
			},
		}
	case SurfaceGroupChat, SurfaceDM:
		if surface.MessageID == "" {
			return nil
		}
		return &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         now.Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				ChatUuid: surface.MessageID,
				Chat: &dgraphStruct.DgraphChat{
					Uuid: surface.MessageID,
					Body: text,
					From: from,
					DM:   &dgraphStruct.DgraphDm{GroupingId: surface.GroupID},
				},
				CreatedAt: &now,
			},
		}
	default:
		return nil // SurfaceTask uses the task-comment notification path
	}
}

// ---------------------------------------------------------------------------
// A delegated task: telling the person who handed it over.
//
// On a task the agent reports by editing one status comment ("On it", then the
// result or its question). A new comment notifies the project, but an edit
// notifies nobody, so the first word people heard was "On it" and the moments
// that mattered, "I need your input" and "done", reached only whoever happened
// to reopen the task. The person who delegated it is the one waiting, so they
// get a push at those moments, and the tap opens the task.

const taskDelegatorBodyMax = 140

var (
	delegatedTaskName = func(ctx context.Context, agent *model.AiAgent, taskID string) string {
		bot, err := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey))
		if err != nil || bot == nil || bot.DgraphUID == "" {
			return ""
		}
		task, terr := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskID, bot.DgraphUID)
		if terr != nil || task == nil {
			return ""
		}
		return task.Name
	}
	pushToDelegator = func(ctx context.Context, userUUID string, data map[string]string) error {
		tokens, err := userFCMTokenBusiness.GetFCMTokenByUserId(ctx, userUUID)
		if err != nil || len(tokens) == 0 {
			return err
		}
		return firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens)
	}
)

// delegatorPushData is what the notification says. Pure.
func delegatorPushData(agentName, taskID, taskName, text string) map[string]string {
	title := strings.TrimSpace(agentName)
	if n := strings.TrimSpace(taskName); n != "" {
		title += " · " + n
	}
	body := strings.Join(strings.Fields(text), " ")
	if r := []rune(body); len(r) > taskDelegatorBodyMax {
		body = strings.TrimSpace(string(r[:taskDelegatorBodyMax-1])) + "…"
	}
	return map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:    firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: taskID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:   title,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:    body,
		firebaseInit.FIREBASE_PUSH_DATA_TAG:     "agent_task_" + taskID,
	}
}

// notifyTaskDelegator pushes a delegated task's moment (a question, or the
// result) to the person who assigned it. Detached and best-effort: a failed
// push never touches the job.
func notifyTaskDelegator(agent *model.AiAgent, taskID, delegatorUUID, text string) {
	delegatorUUID, taskID, text = strings.TrimSpace(delegatorUUID), strings.TrimSpace(taskID), strings.TrimSpace(text)
	if agent == nil || delegatorUUID == "" || taskID == "" || text == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.LogErrorWithContext(context.Background(), "agentNotify: notifyTaskDelegator panic: %v", r)
			}
		}()
		ctx := context.Background()
		data := delegatorPushData(agent.Name, taskID, delegatedTaskName(ctx, agent, taskID), text)
		if err := pushToDelegator(ctx, delegatorUUID, data); err != nil {
			helpers.LogErrorWithContext(ctx, "agentNotify: push to delegator failed (task=%s): %v", taskID, err)
		}
	}()
}
