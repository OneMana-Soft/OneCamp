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
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POST,
		Data: mqttPost,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishPost failed to marshal mqttPost struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishPost", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPost failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

// PublishChannelUpdate notifies every member currently subscribed to a
// channel's message topic that the channel's metadata changed, so their cached
// channel state (post policy, archived flag, membership, name/privacy) is
// revalidated live instead of after a manual refresh. Best-effort and
// idempotent: a dropped or duplicated event simply means the next focus/poll
// reconciles the state.
func PublishChannelUpdate(channelId string, action string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHANNEL_UPDATE,
		Data: &mqttStruct.MqttChannelUpdate{
			Type:        mqttStruct.TYPE_UPDATE,
			ChannelUUID: channelId,
			Action:      action,
		},
	}
	marshalled, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChannelUpdate failed to marshal MqttChannelUpdate struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalled)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChannelUpdate", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelUpdate failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChannelCall(mqttChannelCall *mqttStruct.MqttChannelCall, channelId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHANNEL_CALL,
		Data: mqttChannelCall,
	}
	marshalMqttChannelCall, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PubliPublishChannelCallshPost failed to marshal mqttChannelCall struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalMqttChannelCall)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChannelCall", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelCall failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChatCall(mqttChatCall *mqttStruct.MqttChatCall, grpId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT_CALL,
		Data: mqttChatCall,
	}
	marshalMqttChannelCall, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChatCall failed to marshal mqttChatCall struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmMessage(grpId), 1, false, marshalMqttChannelCall)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChatCall", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatCall failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishPostReaction(mqttPostReaction *mqttStruct.MqttPostReaction, channelId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POST_REACTION,
		Data: mqttPostReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishPostReaction failed to marshal mqttPostReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishPostReaction", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostReactionfailed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishDocCommentReaction(mqttDocCommentReaction *mqttStruct.MqttDocCommentReaction, docId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_DOC_COMMENT_REACTION,
		Data: mqttDocCommentReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishDocCommentReaction failed to marshal mqttDocCommentReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDoc(docId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishDocCommentReaction", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishDocCommentReaction to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishPostCommentReaction(mqttPostCommentReaction *mqttStruct.MqttPostCommentReaction, channelId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POST_COMMENT_REACTION,
		Data: mqttPostCommentReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishPostCommentReaction failed to marshal mqttPostCommentReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishPostCommentReaction", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostCommentReaction to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
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
	if mqttInit.MqttClient == nil {
		// No broker connected (a test, a tool): nobody is listening.
		return
	}
	payload, err := json.Marshal(mqttStruct.Message{Type: kind, Data: data})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/%s marshal err: %+v", what, err)
		return
	}
	res := mqttInit.MqttClient.Publish(topic, 1, false, payload)
	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:publishToTopic", func() {
		_ = res.WaitTimeout(mqttInit.PublishWait)
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/%s publish err: %+v", what, res.Error())
		}
	})
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
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POST_TYPING,
		Data: mqttChannelTyping,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChannelTyping failed to marshal mqttPostReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelTyping(channelId), 0, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChannelTyping", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelTyping to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishDocComment(mqttDocComment *mqttStruct.MqttDocComment, docId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_DOC_COMMENT,
		Data: mqttDocComment,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishDocComment failed to marshal mqttPostComment struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDoc(docId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishDocComment", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishDocComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

// PublishTaskComment tells a task's project about a comment on the task.
func PublishTaskComment(mqttTaskComment *mqttStruct.MqttTaskComment, projectId string) {
	if mqttTaskComment == nil {
		return
	}
	publishToProject(context.Background(), projectId, mqttStruct.MESSAGE_TASK_COMMENT, mqttTaskComment, "PublishTaskComment", mqttTaskComment.TaskUuid)
}

func PublishPostComment(mqttPostCommentCount *mqttStruct.MqttPostComment, channelId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POST_COMMENT,
		Data: mqttPostCommentCount,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishPostComment failed to marshal mqttPostCommentCount struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishPostComment", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChat(mqttChat *mqttStruct.MqttChat, grpId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT,
		Data: mqttChat,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChat failed to marshal mqttChat struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmMessage(grpId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChat", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChat failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChatReaction(mqttChatReaction *mqttStruct.MqttChatReaction, grpId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT_REACTION,
		Data: mqttChatReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChatReaction failed to marshal mqttChatReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmMessage(grpId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChatReaction", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatReaction failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChatCommentReaction(mqttChatCommentReaction *mqttStruct.MqttChatCommentReaction, grpId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT_COMMENT_REACTION,
		Data: mqttChatCommentReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChatCommentReaction failed to marshal mqttChatCommentReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmMessage(grpId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChatCommentReaction", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatCommentReaction failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishChatTyping(mqttChatTyping *mqttStruct.MqttChatTyping, grpId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT_TYPING,
		Data: mqttChatTyping,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChatTyping failed to marshal mqttChatTyping struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmTyping(grpId), 0, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChatTyping", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatTyping to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishUserEmojiStatus(mqttUserEmoji *mqttStruct.MqttUserEmojiStatus) {
	ctx := context.Background()
	mqttMessaage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_USER_EMOJI_STATUS,
		Data: mqttUserEmoji,
	}

	marshalMqttUserEmojiStatus, err := json.Marshal(mqttMessaage)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishUserEmojiStatus failed to marshal mqttUserEmojiStatus struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetPublicUsersStatusTopic(), 1, false, marshalMqttUserEmojiStatus)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishUserEmojiStatus", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishUserEmojiStatus to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})

}

func PublishUserStatus(mqttUserStatus *mqttStruct.MqttUserStatus) {
	ctx := context.Background()
	mqttMessaage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_USER_STATUS,
		Data: mqttUserStatus,
	}

	marshalMqttUserStatus, err := json.Marshal(mqttMessaage)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishUserStatus failed to marshal mqttUserStatus struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetPublicUsersStatusTopic(), 1, false, marshalMqttUserStatus)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishUserStatus", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishUserStatus to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})

}

func PublishChatComment(mqttChatComment *mqttStruct.MqttChatComment, grpId string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_CHAT_COMMENT,
		Data: mqttChatComment,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishChatComment failed to marshal mqttChatComment struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForDmMessage(grpId), 1, false, marshalMqttCreatePost)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishChatComment", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

func PublishActivity(mqttActivity *mqttStruct.MqttActivity, userUUID string) {
	ctx := context.Background()
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_ACTIVITY,
		Data: mqttActivity,
	}
	marshalMqttActivity, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishActivity failed to marshal mqttActivity struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForUserActivity(userUUID), 1, false, marshalMqttActivity)

	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishActivity", func() {
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishActivity failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	})
}

// PublishMessageToUser publishes an arbitrary typed message to a single user's
// activity topic. Generic counterpart to PublishActivity, used by features
// (e.g. proactive nudges) that ride the per-user activity channel with their
// own MESSAGE_* type. Best-effort: marshal/publish errors are logged, never
// propagated, since these are fire-and-forget live updates.
func PublishMessageToUser(userUUID string, msgType int8, data interface{}) {
	ctx := context.Background()
	if mqttInit.MqttClient == nil {
		return
	}
	payload, err := json.Marshal(mqttStruct.Message{Type: msgType, Data: data})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishMessageToUser failed to marshal message err: %+v", err)
		return
	}
	token := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForUserActivity(userUUID), 1, false, payload)
	go func() {
		defer func() { _ = recover() }()
		_ = token.WaitTimeout(mqttInit.PublishWait)
		if token.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishMessageToUser failed to publish to mqtt err: %+v", token.Error())
		}
	}()
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
	ctx := context.Background()
	if mqttInit.MqttClient == nil || len(topics) == 0 {
		return
	}
	payload, err := json.Marshal(mqttStruct.Message{Type: msgType, Data: data})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishToTopics failed to marshal message err: %+v", err)
		return
	}
	sent := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if topic == "" || sent[topic] {
			continue
		}
		sent[topic] = true
		name := topic
		token := mqttInit.MqttClient.Publish(topic, 1, false, payload)
		go func() {
			defer func() { _ = recover() }()
			_ = token.WaitTimeout(mqttInit.PublishWait)
			if token.Error() != nil {
				helpers.LogErrorWithContext(ctx,
					"business/PublishToTopics failed to publish to %s err: %+v", name, token.Error())
			}
		}()
	}
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
			helpers.MessageLogs.ErrorLog.Printf(
				"business/PublishArchiveJobStatus recovered from panic: %v",
				r)
		}
	}()

	if mqttArchiveJob == nil {
		return
	}

	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_ARCHIVE_JOB_STATUS,
		Data: mqttArchiveJob,
	}
	marshalled, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishArchiveJobStatus failed to marshal mqttArchiveJob struct err: %+v",
			err)
		return
	}

	if mqttInit.MqttClient == nil || !mqttInit.MqttClient.IsConnected() {
		// Broker not connected (e.g. dev without EMQX). The FE polling
		// fallback handles this; don't fail loudly.
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForAdminBroadcast(), 1, false, marshalled)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf(
					"business/PublishArchiveJobStatus result-wait recovered from panic: %v",
					r)
			}
		}()
		_ = mqttClientRes.WaitTimeout(mqttInit.PublishWait)
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishArchiveJobStatus failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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
			helpers.MessageLogs.ErrorLog.Printf(
				"business/PublishSlackImportProgress recovered from panic: %v", r)
		}
	}()

	if p == nil {
		return
	}

	ctx := context.Background()
	marshalled, err := importProgressPayload(p)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishSlackImportProgress marshal err: %+v", err)
		return
	}

	if mqttInit.MqttClient == nil || !mqttInit.MqttClient.IsConnected() {
		return
	}

	res := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForAdminBroadcast(), 1, false, marshalled)
	go func() {
		defer func() { _ = recover() }()
		_ = res.WaitTimeout(mqttInit.PublishWait)
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishSlackImportProgress publish err: %+v", res.Error())
		}
	}()
}

// importProgressPayload is an import's progress event as every admin's page
// receives it. Its error goes without URL query strings: a provider's network
// error carries its request's URL, and Trello's carries the key and token in
// it. The caller's event is left as it is. Pure.
func importProgressPayload(p *mqttStruct.MqttSlackImportProgress) ([]byte, error) {
	sent := *p
	sent.ErrorMessage = helpers.WithoutURLQueries(p.ErrorMessage)
	return json.Marshal(mqttStruct.Message{Type: mqttStruct.MESSAGE_SLACK_IMPORT_PROGRESS, Data: &sent})
}

// PublishTableRow broadcasts a data-table row change (create/update/delete) to
// the per-table topic so open grid/board/calendar views update live. data
// should carry the action + row id (+ row for create/update). Fire-and-forget:
// errors are logged, never propagated, and a missing client is a no-op.
func PublishTableRow(tableId string, data interface{}) {
	ctx := context.Background()
	if mqttInit.MqttClient == nil {
		return
	}
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_TABLE_ROW,
		Data: data,
	}
	payload, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/PublishTableRow marshal err: %+v", err)
		return
	}
	res := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForTable(tableId), 1, false, payload)
	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishTableRow", func() {
		_ = res.WaitTimeout(mqttInit.PublishWait)
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/PublishTableRow publish err: %+v", res.Error())
		}
	})
}

// PublishTablePresence tells a table's open pages who is on it and where
// (tables A7), on its presence topic. QoS 0: a lost one is replaced by the
// next, and a page that subscribes late reads the people it answers itself.
func PublishTablePresence(tableId string, data interface{}) {
	ctx := context.Background()
	if mqttInit.MqttClient == nil {
		return
	}
	payload, err := json.Marshal(mqttStruct.Message{Type: mqttStruct.MESSAGE_TABLE_ROW, Data: data})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/PublishTablePresence marshal err: %+v", err)
		return
	}
	res := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForTablePresence(tableId), 0, false, payload)
	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishTablePresence", func() {
		_ = res.WaitTimeout(mqttInit.PublishWait)
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/PublishTablePresence publish err: %+v", res.Error())
		}
	})
}

// PublishPollUpdate tells a channel's clients that one of its polls changed.
func PublishPollUpdate(channelId, pollId string) {
	ctx := context.Background()
	marshalled, err := json.Marshal(mqttStruct.Message{
		Type: mqttStruct.MESSAGE_POLL_UPDATE,
		Data: &mqttStruct.MqttPollUpdate{Type: mqttStruct.TYPE_UPDATE, PollUUID: pollId, ChannelUUID: channelId},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/PublishPollUpdate marshal err: %+v", err)
		return
	}
	res := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForChannelMessage(channelId), 1, false, marshalled)
	helpers.GoSafeNamed("business/Mqtt/mqttBusiness:PublishPollUpdate", func() {
		_ = res.WaitTimeout(mqttInit.PublishWait)
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/PublishPollUpdate publish err: %+v", res.Error())
		}
	})
}
