package business

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func GetMqttConfig(ctx context.Context, userUUID string, isSystemAdmin bool) (mqttConfig *adapter.OutputMqttConfig, err error) {
	mqttWsUrL := os.Getenv("MQTT_WS_URL")
	jwtSecret := os.Getenv("JWT_SECRET")

	// A fresh read, kept for the broker's questions: a client fetches this
	// after joining a channel or starting a conversation, then subscribes.
	member, err := readMemberTopics(ctx, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMqttConfig Failed to get dgraph user infos by uuid err: %+v",
			err,
		)
		return
	}

	var mqttConfigInfo adapter.OutputMqttConfig

	mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, member.list...)
	// Which conversation each is, for the app's subscriptions by conversation
	// (web lib/realtime/subscriptions).
	mqttConfigInfo.ConversationTopics = member.byConversation

	mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, helpers.GetMqttTopicForUserActivity(userUUID))

	mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, helpers.GetPublicUsersStatusTopic())

	// System admins also subscribe to the admin broadcast topic so the archive
	// panel and other admin surfaces receive lifecycle events without polling.
	if isSystemAdmin {
		mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, helpers.GetMqttTopicForAdminBroadcast())
	}

	mqttConfigInfo.Username = "user_" + userUUID

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username": mqttConfigInfo.Username,
		"exp":      time.Now().Add(time.Hour * 24).Unix(),
	})
	mqttPassword, err := token.SignedString([]byte(jwtSecret))

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMqttConfig Failed to create password using jwt token err: %+v",
			err,
		)

		return
	}

	currentTime := time.Now()

	mqttConfigInfo.Password = mqttPassword
	mqttConfigInfo.WsUrl = mqttWsUrL
	mqttConfigInfo.ClientId = mqttConfigInfo.Username + "_" + fmt.Sprintf("%d", currentTime.UnixNano())

	mqttConfig = &mqttConfigInfo

	return
}

func PublishPost(mqttPost *mqttStruct.MqttPost, channelId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_POST, mqttPost, "PublishPost")
}

// PublishChannelUpdate notifies every member currently subscribed to a
// channel's message topic that the channel's metadata changed, so their cached
// channel state (post policy, archived flag, membership, name/privacy) is
// revalidated live instead of after a manual refresh. Best-effort and
// idempotent: a dropped or duplicated event simply means the next focus/poll
// reconciles the state.
func PublishChannelUpdate(channelId string, action string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_CHANNEL_UPDATE,
		&mqttStruct.MqttChannelUpdate{Type: mqttStruct.TYPE_UPDATE, ChannelUUID: channelId, Action: action}, "PublishChannelUpdate")
}

func PublishChannelCall(mqttChannelCall *mqttStruct.MqttChannelCall, channelId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_CHANNEL_CALL, mqttChannelCall, "PublishChannelCall")
}

func PublishChatCall(mqttChatCall *mqttStruct.MqttChatCall, grpId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDmMessage(grpId), mqttStruct.MESSAGE_CHAT_CALL, mqttChatCall, "PublishChatCall")
}

func PublishPostReaction(mqttPostReaction *mqttStruct.MqttPostReaction, channelId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_POST_REACTION, mqttPostReaction, "PublishPostReaction")
}

func PublishDocCommentReaction(mqttDocCommentReaction *mqttStruct.MqttDocCommentReaction, docId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDoc(docId), mqttStruct.MESSAGE_DOC_COMMENT_REACTION, mqttDocCommentReaction, "PublishDocCommentReaction")
}

func PublishPostCommentReaction(mqttPostCommentReaction *mqttStruct.MqttPostCommentReaction, channelId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_POST_COMMENT_REACTION, mqttPostCommentReaction, "PublishPostCommentReaction")
}

// publishToProject sends one message to everyone in a project; tasks are the
// ones it names. Every member hears a project's topic, so a message naming a
// task only a few may see (WA2), even by id, isn't sent: the project is told
// Reread instead, naming nothing, and each member reads its own lists again
// through the filter (models/taskread). Tasks ctx marks open
// (helpers.WithOpenTasks: a batch just made) are named without that read.
func publishToProject(ctx context.Context, projectID string, kind int8, data any, what string, tasks ...string) {
	if projectID == "" {
		return
	}
	if !helpers.TasksOpen(ctx, tasks...) && !ProjectTopicHears(tasks...) {
		kind, data = mqttStruct.MESSAGE_TASK_CHANGED, &mqttStruct.MqttTaskChanged{ProjectUuid: projectID, Reread: true}
	}
	sendToProject(ctx, projectID, kind, data, what)
}

// sendToProject puts one message on a project's topic. A var so a test can
// hear what a project is told.
var sendToProject = func(ctx context.Context, projectID string, kind int8, data any, what string) {
	publishToTopic(ctx, helpers.GetMqttTopicForProjectMessage(projectID), kind, data, what)
}

// publishToTopic sends one message of a kind to a topic. what names the
// publisher in the log.
func publishToTopic(ctx context.Context, topic string, kind int8, data any, what string) {
	publishTo(ctx, []string{topic}, qosAtLeastOnce, kind, data, what)
}

// The delivery a publish asks the broker for: a message at least once, a
// passing signal (typing, presence) at most once.
const (
	qosAtMostOnce  byte = 0
	qosAtLeastOnce byte = 1
)

// publishTo is every publish (D1): the message {kind, data} is marshalled
// once and sent, never retained, to each distinct topic at qos, and a
// goroutine waits for the broker's answer and logs a failure. With no broker
// connected (a test, a tool, a one-shot command) nobody is listening, so it
// sends nothing. what names the publisher in the log.
func publishTo(ctx context.Context, topics []string, qos byte, kind int8, data any, what string) {
	if mqttInit.MqttClient == nil || len(topics) == 0 {
		return
	}
	payload, err := json.Marshal(mqttStruct.Message{Type: kind, Data: data})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/%s marshal err: %+v", what, err)
		return
	}
	sent := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if topic == "" || sent[topic] {
			continue
		}
		sent[topic] = true
		res := mqttInit.MqttClient.Publish(topic, qos, false, payload)
		helpers.GoSafeNamed("business/Mqtt/mqttBusiness:publishTo", func() {
			_ = res.WaitTimeout(mqttInit.PublishWait)
			if res.Error() != nil {
				helpers.LogErrorWithContext(ctx, "business/%s publish to %s err: %+v", what, topic, res.Error())
			}
		})
	}
}

// PublishCallState tells a call's conversation that the call changed: a
// channel's members, a DM's two people or a group's members, on the topics
// the call's dot has always used. A meeting has no conversation to tell, so
// each member in its call hears on their own topic (state.Members): its host
// reads the call again and sees who knocked.
func PublishCallState(state *mqttStruct.MqttCallState) {
	if state == nil || state.ResourceID == "" {
		return
	}
	var topic string
	switch state.ResourceKind {
	case mqttStruct.CONVERSATION_KIND_CHANNEL:
		topic = helpers.GetMqttTopicForChannelMessage(state.ResourceID)
	case mqttStruct.CONVERSATION_KIND_DM, mqttStruct.CONVERSATION_KIND_GROUP:
		topic = helpers.GetMqttTopicForDmMessage(state.ResourceID)
	case mqttStruct.CALL_RESOURCE_MEETING:
		PublishToTopics(activityTopics(state.Members), mqttStruct.MESSAGE_CALL_STATE, state)
		return
	default:
		return
	}
	publishToTopic(context.Background(), topic, mqttStruct.MESSAGE_CALL_STATE, state, "PublishCallState")
}

// ProjectTopicHears is whether a project's topic may name the tasks: not when
// one of them is restricted (WA2), or its restriction can't be read. The one
// check for everything said there: publishToProject, and an agent's work on a
// task (business/AIAgent).
func ProjectTopicHears(tasks ...string) bool {
	return len(restrictedAmong(tasks)) == 0
}

// restrictedAmong is which of the tasks their project keeps to a few people
// (WA2), by lower-case uuid, in one read. A restriction that can't be read
// counts as one. A seam for tests.
var restrictedAmong = func(taskUUIDs []string) map[string]bool {
	ids := make([]uuid.UUID, 0, len(taskUUIDs))
	for _, s := range taskUUIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	found, err := restrictionModel.OfMany(context.Background(), ids)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err != nil || found[id].Restricted {
			out[id.String()] = true
		}
	}
	return out
}

// PublishTaskDates tells a task's project that its dates changed.
func PublishTaskDates(msg *mqttStruct.MqttTaskDates) {
	if msg == nil {
		return
	}
	publishToProject(context.Background(), msg.ProjectUuid, mqttStruct.MESSAGE_TASK_DATES, msg, "PublishTaskDates", msg.TaskUuid)
}

// PublishTaskDatesMany tells a project, in one message, that tasks of it
// moved together (a move and the chain it pushed along, DG10): the first in
// the message's own fields, every one in Tasks. A chain with a task only a
// few may see is told as Reread (publishToProject).
func PublishTaskDatesMany(projectUUID, by string, tasks []mqttStruct.MqttTaskDate) {
	if len(tasks) == 0 {
		return
	}
	named := make([]string, len(tasks))
	for i, t := range tasks {
		named[i] = t.TaskUuid
	}
	first := tasks[0]
	msg := &mqttStruct.MqttTaskDates{TaskUuid: first.TaskUuid, ProjectUuid: projectUUID, StartDate: first.StartDate, DueDate: first.DueDate, By: by, Tasks: tasks}
	publishToProject(context.Background(), projectUUID, mqttStruct.MESSAGE_TASK_DATES, msg, "PublishTaskDatesMany", named...)
}

// PublishTaskChanged tells a task's project that the task changed.
func PublishTaskChanged(ctx context.Context, msg *mqttStruct.MqttTaskChanged) {
	if msg == nil {
		return
	}
	publishToProject(ctx, msg.ProjectUuid, mqttStruct.MESSAGE_TASK_CHANGED, msg, "PublishTaskChanged", msg.TaskUuid)
}

// PublishProjectStatuses tells a project that its statuses, or their order on
// its board, changed: its open boards and pickers read them again.
func PublishProjectStatuses(projectID string) {
	if projectID == "" {
		return
	}
	publishToProject(context.Background(), projectID, mqttStruct.MESSAGE_PROJECT_STATUSES, &mqttStruct.MqttProjectStatuses{ProjectUuid: projectID}, "PublishProjectStatuses")
}

// PublishTaskField tells a task's project that one of its field values changed.
func PublishTaskField(msg *mqttStruct.MqttTaskField) {
	if msg == nil {
		return
	}
	publishToProject(context.Background(), msg.ProjectUuid, mqttStruct.MESSAGE_TASK_FIELD, msg, "PublishTaskField", msg.TaskUuid)
}

// PublishTaskCommentReaction tells a task's project that a reaction on one of
// its comments changed.
func PublishTaskCommentReaction(mqttTaskCommentReaction *mqttStruct.MqttTaskCommentReaction, projectId string) {
	if mqttTaskCommentReaction == nil {
		return
	}
	publishToProject(context.Background(), projectId, mqttStruct.MESSAGE_TASK_COMMENT_REACTION, mqttTaskCommentReaction, "PublishTaskCommentReaction", mqttTaskCommentReaction.TaskUuid)
}

// TypingRepublishInterval is how often a bot still at work says it is typing
// again. The app drops a typing line 6 s after the last message about it
// (onecamp-fe lib/typingTiming.ts), so repeating it every 3 s keeps the line
// up even when one message arrives late. At 4 s each way, the line blinked
// out between messages.
const TypingRepublishInterval = 3 * time.Second

func PublishChannelTyping(mqttChannelTyping *mqttStruct.MqttChannelTyping, channelId string) {
	publishTo(context.Background(), []string{helpers.GetMqttTopicForChannelTyping(channelId)}, qosAtMostOnce, mqttStruct.MESSAGE_POST_TYPING, mqttChannelTyping, "PublishChannelTyping")
}

func PublishDocComment(mqttDocComment *mqttStruct.MqttDocComment, docId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDoc(docId), mqttStruct.MESSAGE_DOC_COMMENT, mqttDocComment, "PublishDocComment")
}

// PublishTaskComment tells a task's project about a comment on the task.
func PublishTaskComment(mqttTaskComment *mqttStruct.MqttTaskComment, projectId string) {
	if mqttTaskComment == nil {
		return
	}
	publishToProject(context.Background(), projectId, mqttStruct.MESSAGE_TASK_COMMENT, mqttTaskComment, "PublishTaskComment", mqttTaskComment.TaskUuid)
}

func PublishPostComment(mqttPostCommentCount *mqttStruct.MqttPostComment, channelId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForChannelMessage(channelId), mqttStruct.MESSAGE_POST_COMMENT, mqttPostCommentCount, "PublishPostComment")
}

func PublishChat(mqttChat *mqttStruct.MqttChat, grpId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDmMessage(grpId), mqttStruct.MESSAGE_CHAT, mqttChat, "PublishChat")
}

func PublishChatReaction(mqttChatReaction *mqttStruct.MqttChatReaction, grpId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDmMessage(grpId), mqttStruct.MESSAGE_CHAT_REACTION, mqttChatReaction, "PublishChatReaction")
}

func PublishChatCommentReaction(mqttChatCommentReaction *mqttStruct.MqttChatCommentReaction, grpId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDmMessage(grpId), mqttStruct.MESSAGE_CHAT_COMMENT_REACTION, mqttChatCommentReaction, "PublishChatCommentReaction")
}

func PublishChatTyping(mqttChatTyping *mqttStruct.MqttChatTyping, grpId string) {
	publishTo(context.Background(), []string{helpers.GetMqttTopicForDmTyping(grpId)}, qosAtMostOnce, mqttStruct.MESSAGE_CHAT_TYPING, mqttChatTyping, "PublishChatTyping")
}

func PublishUserEmojiStatus(mqttUserEmoji *mqttStruct.MqttUserEmojiStatus) {
	publishToTopic(context.Background(), helpers.GetPublicUsersStatusTopic(), mqttStruct.MESSAGE_USER_EMOJI_STATUS, mqttUserEmoji, "PublishUserEmojiStatus")
}

func PublishUserStatus(mqttUserStatus *mqttStruct.MqttUserStatus) {
	publishToTopic(context.Background(), helpers.GetPublicUsersStatusTopic(), mqttStruct.MESSAGE_USER_STATUS, mqttUserStatus, "PublishUserStatus")
}

func PublishChatComment(mqttChatComment *mqttStruct.MqttChatComment, grpId string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForDmMessage(grpId), mqttStruct.MESSAGE_CHAT_COMMENT, mqttChatComment, "PublishChatComment")
}

func PublishActivity(mqttActivity *mqttStruct.MqttActivity, userUUID string) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForUserActivity(userUUID), mqttStruct.MESSAGE_ACTIVITY, mqttActivity, "PublishActivity")
}

// PublishMessageToUser publishes an arbitrary typed message to a single user's
// activity topic. Generic counterpart to PublishActivity, used by features
// (e.g. proactive nudges) that ride the per-user activity channel with their
// own MESSAGE_* type. Best-effort: marshal/publish errors are logged, never
// propagated, since these are fire-and-forget live updates.
func PublishMessageToUser(userUUID string, msgType int8, data interface{}) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForUserActivity(userUUID), msgType, data, "PublishMessageToUser")
}

// PublishToTopics publishes ONE typed message to several topics at once —
// the generic counterpart to PublishMessageToUser for events that belong to a
// place (a channel thread, a project, a chat) as well as to people.
//
// It exists so a feature can reach "everyone looking at this thing" without
// adding another near-identical Publish* function: the payload is marshalled
// once, duplicate/blank topics are skipped, and every publish is fire-and-forget
// (a broker hiccup must never fail the operation that triggered the event).
func PublishToTopics(topics []string, msgType int8, data interface{}) {
	publishTo(context.Background(), topics, qosAtLeastOnce, msgType, data, "PublishToTopics")
}

// PublishConversationAccess tells each of people, on their own activity topic
// and nobody else's, that a conversation or a project became theirs
// (CONVERSATION_ADDED) or stopped being theirs (CONVERSATION_DROPPED).
//
// A client is subscribed only to the topics its settings named when it
// connected. A conversation started, joined or added to since, or a project
// made or added to, isn't among them, so nothing in it streamed until a
// reload; one left or removed from stays among them. This is what makes an
// open app fetch its settings again and subscribe, or let go, without
// reconnecting. Blank and repeated people are skipped; best-effort, like every
// publish here.
func PublishConversationAccess(kind, id, change string, people ...string) {
	if id == "" {
		return
	}
	PublishToTopics(activityTopics(people), mqttStruct.MESSAGE_CONVERSATION_ACCESS,
		&mqttStruct.MqttConversationAccess{Kind: kind, Id: id, Change: change})
}

// activityTopics is each person's activity topic, blanks left out.
// PublishToTopics sends to a repeated one once.
func activityTopics(people []string) []string {
	topics := make([]string, 0, len(people))
	for _, p := range people {
		if p != "" {
			topics = append(topics, helpers.GetMqttTopicForUserActivity(p))
		}
	}
	return topics
}

// PublishGitHubSync tells a task's project what GitHub changed on the task,
// with the new values.
func PublishGitHubSync(mqttGitHubSync *mqttStruct.MqttGitHubSync, projectId string) {
	if mqttGitHubSync == nil {
		return
	}
	publishToProject(context.Background(), projectId, mqttStruct.MESSAGE_GITHUB_SYNC, mqttGitHubSync, "PublishGitHubSync", mqttGitHubSync.TaskUuid)
}

// PublishArchiveJobStatus publishes archive job lifecycle changes to the
// admin broadcast topic. Admin clients use this to update the archive panel
// in real-time, replacing the old 3-second polling loop.
//
// QoS 1 is used to guarantee delivery: missing a "completed" event would
// leave the FE showing "running" indefinitely. QoS 1 may produce duplicates,
// which the FE handles by mutating an SWR cache (idempotent).
//
// Resilient to panics so a publish failure cannot crash the archive worker
// goroutine that called it.
func PublishArchiveJobStatus(mqttArchiveJob *mqttStruct.MqttArchiveJobStatus) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("business/PublishArchiveJobStatus recovered from panic: %v", r)
		}
	}()
	if mqttArchiveJob == nil || mqttInit.MqttClient == nil || !mqttInit.MqttClient.IsConnected() {
		return
	}
	publishToTopic(context.Background(), helpers.GetMqttTopicForAdminBroadcast(), mqttStruct.MESSAGE_ARCHIVE_JOB_STATUS, mqttArchiveJob, "PublishArchiveJobStatus")
}

// PublishSlackImportProgress sends a single Slack-import progress event
// to admin subscribers. Throttling is the caller's responsibility — the
// orchestrator emits at most every 1 second OR every 1000 items, whichever
// fires first, to avoid flooding EMQX during dense imports.
//
// Resilient to broker outages: the publish is fire-and-forget and the
// goroutine that waits on the result will recover from any panic.
func PublishSlackImportProgress(p *mqttStruct.MqttSlackImportProgress) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("business/PublishSlackImportProgress recovered from panic: %v", r)
		}
	}()
	if p == nil || mqttInit.MqttClient == nil || !mqttInit.MqttClient.IsConnected() {
		return
	}
	publishToTopic(context.Background(), helpers.GetMqttTopicForAdminBroadcast(), mqttStruct.MESSAGE_SLACK_IMPORT_PROGRESS, importProgressSent(p), "PublishSlackImportProgress")
}

// importProgressSent is p as it is sent: an error's URLs without their
// queries, which can carry a signed link's token. Pure.
func importProgressSent(p *mqttStruct.MqttSlackImportProgress) *mqttStruct.MqttSlackImportProgress {
	sent := *p
	sent.ErrorMessage = helpers.WithoutURLQueries(p.ErrorMessage)
	return &sent
}

// PublishTableRow broadcasts a data-table row change (create/update/delete) to
// the per-table topic so open grid/board/calendar views update live. data
// should carry the action + row id (+ row for create/update). Fire-and-forget:
// errors are logged, never propagated, and a missing client is a no-op.
func PublishTableRow(tableId string, data interface{}) {
	publishToTopic(context.Background(), helpers.GetMqttTopicForTable(tableId), mqttStruct.MESSAGE_TABLE_ROW, data, "PublishTableRow")
}

// PublishTablePresence tells a table's open pages who is on it and where
// (tables A7), on its presence topic. QoS 0: a lost one is replaced by the
// next, and a page that subscribes late reads the people it answers itself.
func PublishTablePresence(tableId string, data interface{}) {
	publishTo(context.Background(), []string{helpers.GetMqttTopicForTablePresence(tableId)}, qosAtMostOnce, mqttStruct.MESSAGE_TABLE_ROW, data, "PublishTablePresence")
}

// PublishPollUpdate tells a channel's clients that one of its polls changed.
func PublishPollUpdate(channelId, pollId string) {
	publishPollUpdate(helpers.GetMqttTopicForChannelMessage(channelId),
		&mqttStruct.MqttPollUpdate{Type: mqttStruct.TYPE_UPDATE, PollUUID: pollId, ChannelUUID: channelId})
}

// PublishBoardPollUpdate tells a board's open tabs that one of its polls was
// started (TYPE_CREATE) or changed (TYPE_UPDATE: a vote, a close), on the
// board's own topic, which only who may open the board hears
// (business/MqttAccess readsBoard).
func PublishBoardPollUpdate(boardId, pollId, frameId string, kind int8) {
	publishPollUpdate(helpers.GetMqttTopicForBoard(boardId),
		&mqttStruct.MqttPollUpdate{Type: kind, PollUUID: pollId, BoardUUID: boardId, FrameID: frameId})
}

func publishPollUpdate(topic string, update *mqttStruct.MqttPollUpdate) {
	publishToTopic(context.Background(), topic, mqttStruct.MESSAGE_POLL_UPDATE, update, "PublishPollUpdate")
}
