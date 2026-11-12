package business

// Proactive notification to the person who triggered a durable agent run
// (async-mentions: "tag the human when it's blocked or done"). When a background
// run pauses awaiting a human decision — or finishes — the person who @mentioned
// the agent gets an Inbox notice, so they aren't left refreshing a thread. This
// complements the evolving in-thread status comment: the comment is the record,
// this is the nudge to the right individual.
//
// The notice goes through the one way an Inbox notice is told
// (notificationBusiness.Tell): stored, so the Inbox lists it and its badge
// counts it, and published live as the same item. It was a live activity item
// alone, which no list held: the badge counted it until a reload and the Inbox
// never showed it (C2). Surface-agnostic: it names the place from the run's
// Surface, so a new surface is a new case here, not a new pipeline.
// Best-effort and fire-and-forget: a delivery miss never blocks the run's
// state machine.

import (
	"context"
	"fmt"
	"strings"
	"time"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// notifyTriggerer tells the person who triggered a durable run of one of its
// moments, by an Inbox notice of kind (NoticeAgentBlocked, NoticeAgentReplied).
// triggeredBy is their app user uuid (empty → no-op, e.g. a scheduled job with
// no human trigger). text is the short, human message (the block reason or
// the final summary). Best-effort.
//
// The agent's own badged principal is the "from" so the ping reads as coming
// from the teammate. If the triggerer IS the agent's principal (shouldn't
// happen — an agent can't @mention itself into a durable run), it is skipped.
// Only someone who can still see where they asked (reach.seesWhereAsked) is
// pinged: the moment carries the agent's words from there.
func notifyTriggerer(ctx context.Context, agent *model.AiAgent, surface Surface, triggeredBy, kind, text string) {
	triggeredBy = strings.TrimSpace(triggeredBy)
	text = strings.TrimSpace(text)
	if agent == nil || triggeredBy == "" || text == "" {
		return
	}
	bot := agentBotOf(ctx, agent)
	if bot == nil {
		return
	}
	if triggeredBy == bot.UUID || triggeredBy == bot.DgraphUID {
		return // never notify the agent about itself
	}
	n := triggererNotice(agent, bot, surface, kind, text)
	if n == nil {
		return
	}
	sees, err := reach.seesWhereAsked(ctx, triggeredBy, surface)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AIAgent notifyTriggerer could not check whether %s can still see where they asked, so they aren't told: %v", triggeredBy, err)
		return
	}
	if !sees {
		return
	}
	if n.ResourceKind == noticeChannel {
		if names, err := channelNames(ctx, []string{n.ResourceUUID}); err == nil {
			n.ResourceTitle = names[n.ResourceUUID]
		}
	}
	// In the Inbox alone, as the ping always was: no push, no email.
	tellTriggerer(ctx, *n, []notificationBusiness.NoticeRecipient{{UserUUID: triggeredBy, Reason: "subject", InboxOnly: true}})
}

// notifyRunMoment tells a run's triggerer of one of its moments, which the
// run has just shown through status. One answer is one step of their Inbox
// badge: when status told the thread of the answer and that put it in the
// triggerer's Inbox (they wrote the post), the run's "replied" notice to them
// adds nothing and is left out. A blocked moment keeps its notice: it asks
// them for something.
func notifyRunMoment(ctx context.Context, agent *model.AiAgent, surface Surface, status StatusPoster, triggeredBy, kind, text string) {
	if kind == NoticeAgentReplied {
		if t, ok := status.(wordTeller); ok && t.lastWordInInbox(ctx, triggeredBy) {
			return
		}
	}
	notifyTriggerer(ctx, agent, surface, triggeredBy, kind, text)
}

// The Inbox notice kinds of a run's moments: blocked on its triggerer's
// input, or any other word from it (its answer, a stop, a pause).
const (
	NoticeAgentReplied = "agent.replied"
	NoticeAgentBlocked = "agent.blocked"
)

// Seams for tests: writing a moment as its triggerer's Inbox notice, and the
// agent's own principal.
var (
	tellTriggerer = notificationBusiness.Tell
	agentBotOf    = resolveAgentBot
)

// The notices' resource kinds: the place a run was asked in. Each kind's own
// package registers how the Inbox titles and opens it (both editions).
const (
	noticeChannel      = channelBusiness.NoticeKind
	noticeConversation = chatNotificationBusiness.NoticeKind
)

// noticeWordsMax is how much of the agent's words a notice keeps: a line, as
// a push says it.
const noticeWordsMax = 280

// triggererNotice is a run's moment as its triggerer's Inbox notice: from the
// agent, about the channel or the conversation they asked in (the post or the
// message it answers in its data), with the agent's words on one line, which
// the Inbox shows only while they can still open that place (NoticeWords).
// Each moment is a notice of its own. Nil for a surface with no place: the
// task surface has its own path (notifyTaskDelegator). Pure but for the time
// in its key.
func triggererNotice(agent *model.AiAgent, bot *userBusiness.BotIdentity, surface Surface, kind, text string) *notificationBusiness.Notice {
	words := strings.Join(strings.Fields(text), " ")
	if r := []rune(words); len(r) > noticeWordsMax {
		words = strings.TrimSpace(string(r[:noticeWordsMax-1])) + "…"
	}
	n := &notificationBusiness.Notice{
		Kind:  kind,
		Agent: &dgraphStruct.DgraphUser{Uuid: bot.UUID, UserName: agent.Name},
		Data:  map[string]string{notificationBusiness.NoticeWords: words},
	}
	var at string
	switch surface.Kind {
	case SurfaceChannelPost:
		if surface.ChannelID == "" || surface.PostID == "" {
			return nil
		}
		n.ResourceKind, n.ResourceUUID, at = noticeChannel, surface.ChannelID, surface.PostID
		n.Data["post_id"] = surface.PostID
	case SurfaceGroupChat, SurfaceDM:
		if surface.GroupID == "" || surface.MessageID == "" {
			return nil
		}
		n.ResourceKind, n.ResourceUUID, at = noticeConversation, surface.GroupID, surface.MessageID
		n.Data["message_id"] = surface.MessageID
	default:
		return nil
	}
	n.DedupKey = fmt.Sprintf("%s|%s|%s|%d", kind, bot.UUID, at, time.Now().UnixNano())
	return n
}

// channelNames names channels for a notice: a seam for tests.
var channelNames = channelDomain.GetChannelNamesByUUIDs

// ---------------------------------------------------------------------------
// A delegated task: telling the person who handed it over.
//
// On a task the agent reports by editing one status comment ("On it", then the
// result or its question). A new comment notifies the project, but an edit
// notifies nobody, so the first word people heard was "On it" and the moments
// that mattered, "I need your input" and "done", reached only whoever happened
// to reopen the task. The person who delegated it is the one waiting, so they
// are told at those moments, by push and on the desktop app, and the tap opens
// the task.

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
	// Through the one way a push goes out (notificationBusiness.Deliver): their
	// devices and the desktop app, as their pause, quiet hours and focus time
	// allow. Each moment is a notice of its own.
	pushToDelegator = func(ctx context.Context, userUUID, taskID string, data map[string]string) {
		notificationBusiness.Deliver(ctx, []string{userUUID}, data, fmt.Sprintf("agent_task:%s:%d", taskID, time.Now().UnixMilli()))
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
// result) to the person who assigned it, while they may still read the task
// (reach.readsTask): the push names it and carries the agent's words, and a
// run outlasts someone taken off the project or kept out by a restriction
// since. Detached and best-effort: a failed push never touches the job.
func notifyTaskDelegator(agent *model.AiAgent, taskID, delegatorUUID, text string) {
	delegatorUUID, taskID, text = strings.TrimSpace(delegatorUUID), strings.TrimSpace(taskID), strings.TrimSpace(text)
	if agent == nil || delegatorUUID == "" || taskID == "" || text == "" {
		return
	}
	helpers.GoSafeNamed("notifyTaskDelegator", func() {
		ctx := context.Background()
		reads, err := reach.readsTask(ctx, delegatorUUID, taskID)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/AIAgent notifyTaskDelegator could not check whether %s may read task %s, so they aren't told: %v", delegatorUUID, taskID, err)
			return
		}
		if !reads {
			return
		}
		data := delegatorPushData(agent.Name, taskID, delegatedTaskName(ctx, agent, taskID), text)
		pushToDelegator(ctx, delegatorUUID, taskID, data)
	})
}
