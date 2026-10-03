package business

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Mqtt"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/golang-jwt/jwt/v5"
)

func GetMqttConfig(ctx context.Context, userUUID string, isSystemAdmin bool) (mqttConfig *adapter.OutputMqttConfig, err error) {
	mqttWsUrL := os.Getenv("MQTT_WS_URL")
	jwtSecret := os.Getenv("JWT_SECRET")

	dgraphUsers, err := userDomain.GetDgraphUserInfoByUUIDForMQTTConfig(ctx, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMqttConfig Failed to get dgraph user infos by uuid err: %+v",
			err,
		)
		return
	}

	var mqttConfigInfo adapter.OutputMqttConfig

	for _, channel := range dgraphUsers.Channels {
		msgTopic, typingTopic := helpers.GetMqttTopicForChannel(channel.Uuid)
		mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, msgTopic, typingTopic)
	}

	for _, dms := range dgraphUsers.DMs {
		msgTopic, typingTopic := helpers.GetMqttTopicForDm(dms.GroupingId)
		mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, msgTopic, typingTopic)
	}

	// Subscribe to project topics so task comments, reactions, and GitHub sync
	// messages arrive in real-time (these publish to GetMqttTopicForProjectMessage).
	for _, project := range dgraphUsers.Projects {
		mqttConfigInfo.Topics = append(mqttConfigInfo.Topics, helpers.GetMqttTopicForProjectMessage(project.Uuid))
	}

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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPost failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelUpdate failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelCall failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatCall failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostReactionfailed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishDocCommentReaction to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostCommentReaction to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
}

func PublishTaskCommentReaction(mqttTaskCommentReaction *mqttStruct.MqttTaskCommentReaction, projectId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_TASK_COMMENT_REACTION,
		Data: mqttTaskCommentReaction,
	}
	marshalMqttCreatePost, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishTaskCommentReaction failed to marshal mqttPostCommentReaction struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForProjectMessage(projectId), 1, false, marshalMqttCreatePost)

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishTaskCommentReaction to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
}

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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChannelTyping to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishDocComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
}

func PublishTaskComment(mqttTaskComment *mqttStruct.MqttTaskComment, projectId string) {

	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_TASK_COMMENT,
		Data: mqttTaskComment,
	}
	marshalMqttCreateTaskComment, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishTaskComment failed to marshal mqttTaskComment struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForProjectMessage(projectId), 1, false, marshalMqttCreateTaskComment)

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishTaskComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()

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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishPostComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChat failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatReaction failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatCommentReaction failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatTyping to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishUserEmojiStatus to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()

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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishUserStatus to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()

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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishChatComment to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishActivity failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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
		_ = token.Wait()
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
			_ = token.Wait()
			if token.Error() != nil {
				helpers.LogErrorWithContext(ctx,
					"business/PublishToTopics failed to publish to %s err: %+v", name, token.Error())
			}
		}()
	}
}

func PublishGitHubSync(mqttGitHubSync *mqttStruct.MqttGitHubSync, projectId string) {
	ctx := context.Background()

	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_GITHUB_SYNC,
		Data: mqttGitHubSync,
	}
	marshalMqttGitHubSync, err := json.Marshal(mqttMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishGitHubSync failed to marshal mqttGitHubSync struct err: %+v",
			err)
		return
	}

	mqttClientRes := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForProjectMessage(projectId), 1, false, marshalMqttGitHubSync)

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishGitHubSync failed to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()
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
		_ = mqttClientRes.Wait()
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
	mqttMessage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_SLACK_IMPORT_PROGRESS,
		Data: p,
	}
	marshalled, err := json.Marshal(mqttMessage)
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
		_ = res.Wait()
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx,
				"business/PublishSlackImportProgress publish err: %+v", res.Error())
		}
	}()
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
	go func() {
		_ = res.Wait()
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/PublishTableRow publish err: %+v", res.Error())
		}
	}()
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
	go func() {
		_ = res.Wait()
		if res.Error() != nil {
			helpers.LogErrorWithContext(ctx, "business/PublishPollUpdate publish err: %+v", res.Error())
		}
	}()
}
