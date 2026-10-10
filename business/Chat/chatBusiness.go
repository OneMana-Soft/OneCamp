package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	LiveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// GetSignedProfileURL is now in userBusiness

type ChatPagination struct {
	Chats   []*dgraphStruct.DgraphChat `json:"chats,omitempty"`
	HasMore bool                       `json:"has_more"`
}

// replyContextSnippetLen bounds the parent snippet prepended to a reply's
// contextual embedding so a long parent can't dominate the vector.
const replyContextSnippetLen = 200

// resolveReplyParentChat validates a Discord-style inline reply target and
// returns the parent chat's Dgraph node (uid + author + body, for the edge, the
// MQTT preview, and the contextual embedding), or nil when the reference is
// invalid. A reply is SAME-CONVERSATION: the parent must exist, be non-deleted,
// and live in the SAME DM/group (identical grouping id). On any failure it
// returns nil so the chat is still created as a normal message and a
// bad/foreign uuid never leaks a parent's existence or content.
func resolveReplyParentChat(ctx context.Context, replyToUUID string, groupingID string) *dgraphStruct.DgraphChat {
	if replyToUUID == "" {
		return nil
	}
	if _, perr := uuid.Parse(replyToUUID); perr != nil {
		return nil
	}
	parent, err := domain.GetDgraphChatOnlyTextByUUID(ctx, replyToUUID)
	if err != nil || parent == nil || parent.Uid == "" {
		return nil
	}
	// Same-conversation guard: the parent must belong to this grouping id.
	// This is the authoritative access check — a uuid from another DM/group
	// (which the sender may not even be part of) has a different grouping id
	// and is rejected, so no cross-conversation content can be referenced.
	if parent.DM == nil || parent.DM.GroupingId != groupingID {
		return nil
	}
	// Not-deleted guard.
	if parent.DeletedAt != nil && !parent.DeletedAt.IsZero() {
		return nil
	}
	return parent
}

// withChatReplyContext prepends a compact "Replying to <author>: <snippet>"
// prefix to a reply's text for the k-NN embedding, so short replies carry the
// meaning of the message they answer. Returns text unchanged when parent is nil.
func withChatReplyContext(parent *dgraphStruct.DgraphChat, text string) string {
	if parent == nil {
		return text
	}
	author := ""
	if parent.From != nil {
		author = parent.From.DisplayName()
	}
	snippet := helpers.HTMLToPlainText(parent.Body)
	if len(snippet) > replyContextSnippetLen {
		snippet = helpers.TruncateRunes(snippet, replyContextSnippetLen)
	}
	if author == "" && snippet == "" {
		return text
	}
	return fmt.Sprintf("Replying to %s: %s\n%s", author, snippet, text)
}

// replyEdgeChat builds the minimal Dgraph edge node (uid only) that links a
// new chat to its validated reply parent, or nil when there is no parent.
func replyEdgeChat(parent *dgraphStruct.DgraphChat) *dgraphStruct.DgraphChat {
	if parent == nil {
		return nil
	}
	return &dgraphStruct.DgraphChat{Uid: parent.Uid}
}

// replyParentChatAuthorUUID returns the parent chat author's UUID (for the
// implicit reply ping), or "" when there is no valid parent / author.
func replyParentChatAuthorUUID(parent *dgraphStruct.DgraphChat) string {
	if parent == nil || parent.From == nil {
		return ""
	}
	return parent.From.Uuid
}

func CreateChatForGroup(ctx context.Context, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, mentions []*dgraphStruct.DgraphUser, participantsDgraphRaw []*dgraphStruct.DgraphUser) (createdChatInfo *adapter.OutputCreateChatForChat, err error) {
	chatUUID := uuid.New()

	var chatMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentions {
		chatMentions = append(chatMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	for _, mediaObj := range chatInfo.MediaObjects {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	var participantsDgraph []*dgraphStruct.DgraphUser
	var userUUIDs []string

	isNewGroup := len(chatInfo.GrpUuid) == 0

	var openSerachChatParticipants []*openSearchStruct.OpenSearchChatParticipants
	if len(participantsDgraphRaw) > 0 {
		for _, u := range participantsDgraphRaw {

			openSerachChatParticipants = append(openSerachChatParticipants, &openSearchStruct.OpenSearchChatParticipants{
				Uuid:       u.Uuid,
				Name:       u.DisplayName(),
				ProfileKey: u.ProfileKey,
			})
			userUUIDs = append(userUUIDs, u.Uuid)

			if isNewGroup {
				// userUUIDs = append(userUUIDs, u.Uuid) Dependeig on pront end need to do something
				participantsDgraph = append(participantsDgraph, &dgraphStruct.DgraphUser{
					Uid: u.Uid,
					DMs: []*dgraphStruct.DgraphDm{
						{
							Uid: "uid(dm)",
						},
					},
				})
			}

		}

	}

	if isNewGroup {
		chatInfo.GrpUuid, err = helpers.GenerateGroupID(userUUIDs)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateChatForGroup Failed to generate grp id err: %+v",
				err)
			return
		}
	}

	err = domain.CreateChat(ctx, chatUUID, userInfo.UserPostgresInfo.Id, chatInfo.GrpUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatForGroup Failed to create new chat err: %+v",
			err)
		return
	}

	// Inline reply (Discord-style): resolve + validate the reply target in the
	// SAME group (identical grouping id). Invalid / foreign / deleted targets
	// resolve to nil and are silently dropped — see resolveReplyParentChat.
	replyParentChat := resolveReplyParentChat(ctx, chatInfo.ReplyToUuid, chatInfo.GrpUuid)

	plainText := helpers.HTMLToPlainText(chatInfo.TextHtml)

	dgraphDm := dgraphStruct.DgraphDm{
		Uid:          "uid(dm)",
		GroupingId:   chatInfo.GrpUuid,
		DType:        []string{"Dm"},
		Participants: participantsDgraph,
		Chats: []*dgraphStruct.DgraphChat{
			{
				Uid:       "uid(ch)",
				Uuid:      chatUUID.String(),
				DType:     []string{"Chat"},
				CreatedAt: &currentTime,
				DeletedAt: &zeroUnixTime,
				From: &dgraphStruct.DgraphUser{
					Uid: userInfo.UserDgraphInfo.Uid,
				},
				DM: &dgraphStruct.DgraphDm{
					Uid: "uid(dm)",
				},
				MediaObj:    chatInfo.MediaObjects,
				Body:        chatInfo.TextHtml,
				ReplyToChat: replyEdgeChat(replyParentChat),
				Mentions: &dgraphStruct.DgraphMentions{
					Mentions:  chatMentions,
					ChatUuid:  chatUUID.String(),
					CreatedAt: &currentTime,
					Chat: &dgraphStruct.DgraphChat{
						Uid: "uid(ch)",
					},
				},
			},
		},
	}

	_, dmUID, err := domain.CreateDgraphChat(ctx, &dgraphDm, chatUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatForGroup Failed to create new chat in dgraph err: %+v",
			err)

		// Reverse the Postgres row and return the ORIGINAL error — see the note in
		// business/CreatePost. Assigning the rollback's result into err made a successful
		// rollback look like a successful send, so the caller replied 200 for a group message
		// that had just been deleted.
		_ = helpers.CompensateOnFailure(ctx, "postgres chat row "+chatUUID.String(),
			func(compensateCtx context.Context) error {
				return domain.HardDeleteChatByUUID(compensateCtx, chatUUID)
			})

		return

	}

	if len(dmUID) != 0 {
		// The others haven't seen the new conversation: their marks start at
		// nothing (it counts as unread, and no read receipt says otherwise).
		err = lastseenBusiness.BulkCreateLastSeenChatIfNotExists(ctx, userUUIDs, chatInfo.GrpUuid)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateChatForGroup Failed to update send to last seen chat err: %+v",
				err)
			return
		}

		chatNotificationBusiness.BulkCreateChatNotification(ctx, userUUIDs, chatInfo.GrpUuid, postgressStruct.NOTIFICATION_TYPE_ALL, currentTime)

	}

	err = lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, chatInfo.GrpUuid, userInfo.UserPostgresInfo.Id, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCCreateChatForGrouphat Failed to update last seen chat err: %+v",
			err)
		return
	}

	createdChatInfo = &adapter.OutputCreateChatForChat{
		Uuid:        chatUUID.String(),
		ChatCreated: currentTime,
	}

	mqttChat := mqttStruct.MqttChat{
		Type:             mqttStruct.TYPE_CREATE,
		ChatUuid:         chatUUID.String(),
		ChatCreatedAt:    &currentTime,
		ChatByProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		ChatByUserName:   userInfo.UserDgraphInfo.DisplayName(),
		ChatByUserUuid:   userInfo.UserDgraphInfo.Uuid,
		ChatByIsBot:      userInfo.UserDgraphInfo.IsBot,
		ChatHtmlText:     chatInfo.TextHtml,
		ChatAttachments:  chatInfo.MediaObjects,
		ChatGrpId:        chatInfo.GrpUuid,
		ChatReplyTo:      replyParentChat,
	}

	bulk := helpers.IsBulkImport(ctx)

	// Pre-compute participant UUID list — used both by AI embedding
	// (skipped during bulk) and by the chat.created webhook below.
	var chatParticipantUUIDs []string
	for _, p := range openSerachChatParticipants {
		chatParticipantUUIDs = append(chatParticipantUUIDs, p.Uuid)
	}

	if !bulk {
		go mqttBusiness.PublishChat(&mqttChat, chatInfo.GrpUuid)
	}

	openSearchChat := &openSearchStruct.OpenSearchChat{
		Uuid:               chatUUID.String(),
		ChatCreatedAt:      currentTime.Unix(),
		ChatBody:           plainText,
		ChatByProfile:      userInfo.UserDgraphInfo.ProfileKey,
		ChatByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		ChatByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		ChatGrpId:          chatInfo.GrpUuid,
		ChatParticipants:   openSerachChatParticipants,
		ChatDeletedAt:      nil,
	}

	pushTitle := fmt.Sprintf("Group - %s", userInfo.UserDgraphInfo.DisplayName())

	if !bulk {
		go sendNewChatNotification(pushTitle, plainText, userInfo.UserDgraphInfo.Uuid, chatInfo.GrpUuid, mentions, chatUUID.String(), userInfo.UserDgraphInfo.DisplayName(), userInfo.UserDgraphInfo.ProfileKey, replyParentChatAuthorUUID(replyParentChat))
	}
	go domain.CreateChatWithAttachmentsInOpenSearch(openSearchChat, chatInfo.MediaObjects)

	if !bulk {
		// embed for AI Second Brain (async) — group chat. Skipped during
		// imports; a follow-up batch embed run handles imported content.
		// For a reply, embed the parent snippet + reply text (contextual
		// embedding); same-conversation guarantees no permission leak.
		ai.EmbedChatContent(withChatReplyContext(replyParentChat, plainText), chatUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(), chatInfo.GrpUuid, userInfo.UserDgraphInfo.Uuid, "", chatParticipantUUIDs)
	}

	if !bulk {
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.created", map[string]interface{}{
			"message_id":      chatUUID.String(),
			"sender_id":       userInfo.UserDgraphInfo.Uuid,
			"group_id":        chatInfo.GrpUuid,
			"text":            plainText,
			"participant_ids": chatParticipantUUIDs,
			"mention_ids":     chatMentionNodeIDs(mentions),
		})
	}

	return

}

func AddParticipantToGroupChat(ctx context.Context, oldGrpId string, newGrpId string, newUserDgraphUID string, newUserUUID string) (err error) {

	err = domain.BulkUpdateGrpIdInChatAndAtachment(ctx, oldGrpId, newGrpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddParticipantToGroupChat Failed to update grpID in chats and attachments err: %+v",
			err)
		return
	}

	dgraphDM := &dgraphStruct.DgraphDm{
		Uid:        "uid(dm)",
		GroupingId: newGrpId,
		Participants: []*dgraphStruct.DgraphUser{
			{
				Uid: newUserDgraphUID,
			},
		},
	}

	_, _, err = domain.CreateDM(ctx, dgraphDM, oldGrpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddParticipantToGroupChat Failed to update grpID in dgraph err: %+v",
			err)
		return
	}

	go domain.MigrateChatGroupIDInOpensearch(oldGrpId, newGrpId)

	// Migrate AI embeddings: update chat_grp_id and append new participant to chat_participant_uuids
	ai.MigrateEmbeddingsGroupIDAsync(oldGrpId, newGrpId, newUserUUID)

	return

}
func CreateChat(ctx context.Context, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, sendToDgraph *dgraphStruct.DgraphUser, toUUID uuid.UUID, mentions []*dgraphStruct.DgraphUser) (createdChatInfo *adapter.OutputCreateChatForChat, err error) {

	// create channel uuid
	chatUUID := uuid.New()

	var chatMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentions {
		chatMentions = append(chatMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	for _, mediaObj := range chatInfo.MediaObjects {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	groupingId := helpers.GetGroupingId(userInfo.UserPostgresInfo.Id.String(), chatInfo.ToUuid)

	// create chat in postgres
	err = domain.CreateChat(ctx, chatUUID, userInfo.UserPostgresInfo.Id, groupingId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChat Failed to create new chat err: %+v",
			err)
		return
	}

	// Inline reply (Discord-style): resolve + validate the reply target in the
	// SAME DM. Invalid / foreign / deleted targets resolve to nil and are
	// silently dropped (the chat is still created) — see resolveReplyParentChat.
	replyParentChat := resolveReplyParentChat(ctx, chatInfo.ReplyToUuid, groupingId)

	plainText := helpers.HTMLToPlainText(chatInfo.TextHtml)

	dgraphDm := &dgraphStruct.DgraphDm{
		Uid:        "uid(dm)",
		GroupingId: groupingId,
		Participants: []*dgraphStruct.DgraphUser{
			{
				Uid: userInfo.UserDgraphInfo.Uid,
				DMs: []*dgraphStruct.DgraphDm{
					{
						Uid: "uid(dm)",
					},
				},
			},
			{

				Uid: sendToDgraph.Uid,
				DMs: []*dgraphStruct.DgraphDm{
					{
						Uid: "uid(dm)",
					},
				},
			},
		},
		DType: []string{"Dm"},
		Chats: []*dgraphStruct.DgraphChat{
			{
				Uid:       "uid(ch)",
				Uuid:      chatUUID.String(),
				DType:     []string{"Chat"},
				CreatedAt: &currentTime,
				DeletedAt: &zeroUnixTime,
				To: &dgraphStruct.DgraphUser{
					Uid: sendToDgraph.Uid,
				},
				From: &dgraphStruct.DgraphUser{
					Uid: userInfo.UserDgraphInfo.Uid,
				},
				DM: &dgraphStruct.DgraphDm{
					Uid: "uid(dm)",
				},
				MediaObj:    chatInfo.MediaObjects,
				Body:        chatInfo.TextHtml,
				ReplyToChat: replyEdgeChat(replyParentChat),
				Mentions: &dgraphStruct.DgraphMentions{
					Mentions:  chatMentions,
					ChatUuid:  chatUUID.String(),
					CreatedAt: &currentTime,
					Chat: &dgraphStruct.DgraphChat{
						Uid: "uid(ch)",
					},
				},
			},
		},
	}

	_, dmUID, err := domain.CreateDgraphChat(ctx, dgraphDm, chatUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChat Failed to create new chat in dgraph err: %+v",
			err)

		// Reverse the Postgres row and return the ORIGINAL error — see the note in
		// business/CreatePost. This is the 1:1 DM path; the same clobbering made a failed send
		// report 200 with a null payload.
		_ = helpers.CompensateOnFailure(ctx, "postgres chat row "+chatUUID.String(),
			func(compensateCtx context.Context) error {
				return domain.HardDeleteChatByUUID(compensateCtx, chatUUID)
			})

		return

	}

	if len(dmUID) != 0 {
		// The other person hasn't seen the new conversation: see CreateChatForGroup.
		err = lastseenBusiness.BulkCreateLastSeenChatIfNotExists(ctx, []string{toUUID.String()}, groupingId)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateChat Failed to update send to last seen chat err: %+v",
				err)
			return
		}
		if userInfo.UserDgraphInfo.Uuid != sendToDgraph.Uuid {
			chatNotificationBusiness.CreateChatNotificationType(ctx, userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid, groupingId, postgressStruct.NOTIFICATION_TYPE_ALL, currentTime)

		}
	}

	err = lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, groupingId, userInfo.UserPostgresInfo.Id, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChat Failed to update last seen chat err: %+v",
			err)
		return
	}

	createdChatInfo = &adapter.OutputCreateChatForChat{
		Uuid:        chatUUID.String(),
		ChatCreated: currentTime,
	}

	mqttChat := mqttStruct.MqttChat{
		Type:             mqttStruct.TYPE_CREATE,
		ChatUuid:         chatUUID.String(),
		ChatCreatedAt:    &currentTime,
		ChatByProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		ChatByUserName:   userInfo.UserDgraphInfo.DisplayName(),
		ChatByUserUuid:   userInfo.UserDgraphInfo.Uuid,
		ChatByIsBot:      userInfo.UserDgraphInfo.IsBot,
		ChatHtmlText:     chatInfo.TextHtml,
		ChatAttachments:  chatInfo.MediaObjects,
		ChatGrpId:        groupingId,
		ChatReplyTo:      replyParentChat,
	}

	bulk := helpers.IsBulkImport(ctx)
	if !bulk {
		go mqttBusiness.PublishChat(&mqttChat, groupingId)
	}

	openSearchChat := &openSearchStruct.OpenSearchChat{
		Uuid:               chatUUID.String(),
		ChatCreatedAt:      currentTime.Unix(),
		ChatBody:           plainText,
		ChatByProfile:      userInfo.UserDgraphInfo.ProfileKey,
		ChatByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		ChatByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		ChatGrpId:          groupingId,
		ChatParticipants: []*openSearchStruct.OpenSearchChatParticipants{{
			Uuid:       userInfo.UserDgraphInfo.Uuid,
			Name:       userInfo.UserDgraphInfo.DisplayName(),
			ProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		},
			{
				Uuid:       sendToDgraph.Uuid,
				Name:       sendToDgraph.DisplayName(),
				ProfileKey: sendToDgraph.ProfileKey,
			}},
		ChatDeletedAt: nil,
	}

	pushTitle := fmt.Sprintf("Dm - %s", userInfo.UserDgraphInfo.DisplayName())

	if !bulk {
		go sendNewChatNotification(pushTitle, plainText, userInfo.UserDgraphInfo.Uuid, groupingId, mentions, chatUUID.String(), userInfo.UserDgraphInfo.DisplayName(), userInfo.UserDgraphInfo.ProfileKey, replyParentChatAuthorUUID(replyParentChat))
	}
	go domain.CreateChatWithAttachmentsInOpenSearch(openSearchChat, chatInfo.MediaObjects)

	if !bulk {
		// embed for AI Second Brain (async) — DM chat. Skipped during
		// bulk imports. For a reply, embed the parent snippet + reply text
		// (contextual embedding) so terse replies carry the meaning of what
		// they answer; same-conversation guarantees no permission leak.
		ai.EmbedChatContent(withChatReplyContext(replyParentChat, plainText), chatUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(), groupingId, userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid, []string{userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid})

		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.created", map[string]interface{}{
			"message_id":  chatUUID.String(),
			"sender_id":   userInfo.UserDgraphInfo.Uuid,
			"receiver_id": sendToDgraph.Uuid,
			"group_id":    groupingId,
			"text":        plainText,
			"mention_ids": chatMentionNodeIDs(mentions),
		})
	}

	return
}

// chatMentionNodeIDs extracts the Dgraph node uids of mentioned users so a
// chat.created listener (e.g. the AI coworker) can detect mentions, mirroring
// the post.created event's mention_ids.
func chatMentionNodeIDs(mentions []*dgraphStruct.DgraphUser) []string {
	out := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m == nil {
			continue
		}
		if m.Uid != "" {
			out = append(out, m.Uid)
		}
		if m.Uuid != "" {
			out = append(out, m.Uuid)
		}
	}
	return out
}

func sendNewChatNotification(title string, body string, userId string, grpId string, mentions []*dgraphStruct.DgraphUser, chatUUID string, username string, profileKey *string, replyToAuthorUUID string) {

	ctx := context.Background()

	var mentionUUDs []string

	for _, m := range mentions {
		mentionUUDs = append(mentionUUDs, m.Uuid)
	}

	// A Discord-style inline reply implicitly pings the parent's author: treat
	// them as a mention recipient (activity broadcast + email priority).
	// Suppress self-replies and de-dup with an explicit @mention.
	replyPing := replyToAuthorUUID != "" && replyToAuthorUUID != userId
	if replyPing {
		alreadyMentioned := false
		for _, u := range mentionUUDs {
			if u == replyToAuthorUUID {
				alreadyMentioned = true
				break
			}
		}
		if !alreadyMentioned {
			mentionUUDs = append(mentionUUDs, replyToAuthorUUID)
		}
	}

	// 1. Get eligible users for notifications (based on preferences and grouping)
	eligibleUserIDs, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grpId, userId, mentionUUDs, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatNotification Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Publish MQTT Activity Events
	mentionsMap := make(map[string]bool)
	for _, m := range mentions {
		mentionsMap[m.Uuid] = true
	}
	// Include the replied-to author so they receive the mention-style activity
	// broadcast for the reply.
	if replyPing {
		mentionsMap[replyToAuthorUUID] = true
	}

	for _, recipientID := range eligibleUserIDs {
		// Only send the activity broadcast if the user was explicitly mentioned
		if !mentionsMap[recipientID] {
			continue
		}

		// Create a simplified activity item for MQTT
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
			Time:         time.Now().Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				ChatUuid: chatUUID,
				Chat: &dgraphStruct.DgraphChat{
					Uuid: chatUUID,
					Body: body,
					From: &dgraphStruct.DgraphUser{
						Uuid:     userId,
						UserName: username,
					},
					DM: &dgraphStruct.DgraphDm{
						GroupingId: grpId,
					},
				},
				CreatedAt: helpers.TimePointer(time.Now()),
			},
		}
		activityBusiness.PublishActivityToUser(recipientID, activityItem)
	}

	// 3. Handle FCM Push Notifications
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatNotification Failed to get user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = chatUUID
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = username
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, profileKey)

	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendNewChatNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// 4. Email fan-out (no-op when RESEND_API_KEY is unset). Same eligibility
	//    set as FCM; the dispatcher applies its own per-recipient gates
	//    (online check, prefs, suppressions, quiet hours).
	notificationBusiness.DispatchChatDM(
		userId,
		username,
		userBusiness.GetSignedProfileURL(ctx, profileKey),
		grpId,
		chatUUID,
		body,
		eligibleUserIDs,
		len(mentions) > 0 || replyPing,
	)

}

func UpdateChat(ctx context.Context, chatInfo *adapter.ChatInfo, chatUUID uuid.UUID, grpId string, chatCreatedBy string, mentions []*dgraphStruct.DgraphUser) (err error) {

	err = domain.UpdateChatByUUID(ctx, chatUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChat Failed to update chat err: %+v",
			err)
		return
	}
	var chatMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentions {
		chatMentions = append(chatMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	currentTime := time.Now()

	dgraphChat := dgraphStruct.DgraphChat{
		Uid:       "uid(cha)",
		Uuid:      chatInfo.Uuid,
		Body:      chatInfo.TextHtml,
		MediaObj:  chatInfo.MediaObjects,
		UpdatedAt: &currentTime,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid:      "uid(me)",
			ChatUuid: chatInfo.Uuid,
			Chat: &dgraphStruct.DgraphChat{
				Uid: "uid(cha)",
			},
			Mentions:  chatMentions,
			UpdatedAt: &currentTime,
		},
	}

	err = domain.UpdateDgraphChat(ctx, &dgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChat Failed to update chat in dgraph err: %+v",
			err)

		return

	}

	mqttChat := mqttStruct.MqttChat{
		Type:           mqttStruct.TYPE_UPDATE,
		ChatUuid:       chatUUID.String(),
		ChatUpdatedAt:  &currentTime,
		ChatHtmlText:   chatInfo.TextHtml,
		ChatByUserUuid: chatCreatedBy,
		ChatGrpId:      grpId,
	}

	plainText := helpers.HTMLToPlainText(chatInfo.TextHtml)
	openSearchChat := &openSearchStruct.OpenSearchChat{
		Uuid:          chatUUID.String(),
		ChatUpdatedAt: currentTime.Unix(),
		ChatBody:      plainText,
	}
	go domain.UpdateChatInOpenSearch(openSearchChat)

	// Suppress live fan-out during a bulk Slack import. The Postgres +
	// Dgraph + OpenSearch writes above land so search and timelines are
	// consistent; the MQTT/AI/webhook events are skipped to avoid
	// notifying every participant of historical edits.
	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishChat(&mqttChat, grpId)

	// Re-embed for AI Second Brain with updated content (async)
	ai.EmbedChatContent(plainText, chatUUID.String(), chatCreatedBy, "", grpId, chatCreatedBy, "", nil)

	// Reconcile any workspace-memory items captured from this DM/group
	// message: refresh the stored snapshot to the edited text, or drop it
	// if the edit emptied the message. (Mirrors UpdatePost.)
	ai.InvalidateMemoryForEditedSourceAsync("chat", chatUUID.String(), plainText)

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.updated", map[string]interface{}{
		"message_id": chatUUID.String(),
	})

	return
}

func DeleteChat(ctx context.Context, chatInfo *adapter.ChatInfo, chatUUID uuid.UUID, grpId string, rawDgraphChat *dgraphStruct.DgraphChat) (err error) {

	err = domain.SoftDeleteChatByUUIUD(ctx, chatUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteComment Failed to soft delete chat err: %+v",
			err)
		return
	}

	currentTime := time.Now()

	dgraphChat := dgraphStruct.DgraphChat{
		Uid:       "uid(cha)",
		Uuid:      chatInfo.Uuid,
		DeletedAt: &currentTime,
	}

	err = domain.UpdateDgraphChat(ctx, &dgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePost Failed to update post in dgraph err: %+v",
			err)

		return

	}

	mqttChat := mqttStruct.MqttChat{
		Type:           mqttStruct.TYPE_DELETE,
		ChatUuid:       chatUUID.String(),
		ChatByUserUuid: rawDgraphChat.From.Uuid,
		ChatGrpId:      grpId,
	}

	openSearchChat := &openSearchStruct.OpenSearchChat{
		Uuid:          chatUUID.String(),
		ChatDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	go domain.DeleteChatWithAttachmentsInOpenSearch(openSearchChat, rawDgraphChat.MediaObj)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_chat_id", "attachment_chat_id"},
		chatUUID.String(),
		currentTime.Unix(),
		[]string{"comments", "attachments"},
		"cascade",
	)

	if helpers.IsBulkImport(ctx) {
		// Skip live MQTT/AI cascade/webhook fan-out during bulk Slack
		// import; the data layer is already consistent.
		return
	}

	go mqttBusiness.PublishChat(&mqttChat, grpId)

	// Cascade soft-delete to AI embeddings (chat + its comments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid", "chat_uuid"},
		chatUUID.String(),
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)

	// Cascade into the Workspace Memory layer: drop any items a user
	// captured FROM this DM/group message so deleted content can't linger
	// in memory or resurface via AI/graph. (Mirrors DeletePost/DeleteComment.)
	ai.DeleteMemoryBySourceAsync("chat", chatUUID.String())

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.deleted", map[string]interface{}{
		"message_id": chatUUID.String(),
	})

	return
}

func GetDgraphChatByUUID(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	dgraphChat, err = domain.GetDgraphChatByUUID(ctx, chatUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChatByUUID Failed to get chat dgraph by chatUUID err: %+v",
			err)

		return
	}
	return
}

func GetDgraphChatBasicByUUID(ctx context.Context, chatUUID string, userDgraphUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	dgraphChat, err = domain.GetDgraphChatBasicByUUID(ctx, chatUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChatBasicByUUID Failed to get chat dgraph by chatUUID err: %+v",
			err)

		return
	}
	return
}

func GetDgraphChatOnlyTextByUUID(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	dgraphChat, err = domain.GetDgraphChatOnlyTextByUUID(ctx, chatUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChatOnlyTextByUUID Failed to get chat dgraph by chatUUID err: %+v",
			err)

		return
	}
	return
}

func GetUserListWithLatestChatWithUserIdAndSearchText(ctx context.Context, userUUID string, searchText string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetUserListWithLatestChatWithUserIdAndSearchText(ctx, userUUID, searchText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserListWithLatestChatWithUserIdAndSearchText Failed to get users list err: %+v",
			err)

		return
	}
	return
}

func GetUserChatListWithLatestChat(ctx context.Context, userUUID uuid.UUID) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetUserChatListWithLatestChat(ctx, userUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserChatListWithLatestChat Failed to get users latest chat err: %+v",
			err)

		return
	}

	chatMessageCount, err := domain.GetLatestChatMessageCountByUserID(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserChatListWithLatestChat Failed to get unread chat count err: %+v",
			err)

		return
	}

	for ind := range dgraphUser.DMs {
		if countProxy, ok := chatMessageCount[dgraphUser.DMs[ind].GroupingId]; ok && countProxy != nil {
			dgraphUser.DMs[ind].UnreadMessageCount = countProxy.ChatCount
		} else {
			dgraphUser.DMs[ind].UnreadMessageCount = 0
		}
	}

	return
}

func GetDgraphOldGroupChatFromDgraph(ctx context.Context, lastChatTime time.Time, grpID string) (chats ChatPagination, err error) {
	dgraphChats, err := domain.GetDgraphOldChatFromDgraph(ctx, grpID, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphOldGroupChatFromDgraph Failed to get old chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphOldChatFromDgraph(ctx context.Context, lastChatTime time.Time, firstUserUUID string, secondUserUUID string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(firstUserUUID, secondUserUUID)
	dgraphChats, err := domain.GetDgraphOldChatFromDgraph(ctx, groupingId, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphOldChatFromDgraph Failed to get old chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphNewGroupChatFromDgraph(ctx context.Context, lastChatTime time.Time, grpId string) (chats ChatPagination, err error) {
	dgraphChats, err := domain.GetDgraphNewChatFromDgraph(ctx, grpId, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewGroupChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphNewChatFromDgraph(ctx context.Context, lastChatTime time.Time, fromUserId string, toUserId string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(fromUserId, toUserId)
	dgraphChats, err := domain.GetDgraphNewChatFromDgraph(ctx, groupingId, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphDmBasicInfoFromDgraph(ctx context.Context, userId string, groupingId string) (dgraphDm *dgraphStruct.DgraphDm, err error) {

	dgraphDm, err = domain.GetDgraphDmBasicInfoFromDgraph(ctx, userId, groupingId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphDmBasicInfoFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	return

}

func GetChatCallStatus(ctx context.Context, grpId string) (exists bool, err error) {

	// Human-participant check, not room existence: the transcriber agent
	// and LiveKit's EmptyTimeout both keep the room object alive after the
	// last human leaves, which would keep the group-chat call dot lit.
	exists, err = LiveKitBusiness.IsCallActive(ctx, grpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChatCallStatus Failed to get chat call status err: %+v",
			err)

		return
	}

	return
}
func GetDMRecordingTranscript(ctx context.Context, groupingId string, userId string, egressId string, pageIndex int, pageSize int) (dgraphDm *dgraphStruct.DgraphDm, err error) {

	dgraphDm, err = domain.GetDMRecordingTranscript(ctx, groupingId, userId, egressId, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDMRecordingTranscript Failed to get chat recording transcript from dgraph err: %+v",
			err)

		return
	}

	return
}

type RecordingPagination struct {
	Recordings          []*dgraphStruct.DgraphRecording `json:"recordings,omitempty"`
	HasMore             bool                            `json:"has_more"`
	ParticipantIsMember int                             `json:"participant_is_member"`
}

func GetDgraphDmRecordingListFromDgraph(ctx context.Context, userId string, groupingId string, startDate string, endDate string, pageIndex int, pageSize int) (recordingsPagination RecordingPagination, err error) {
	dgraphDm, err := domain.GetDgraphDmRecordingListFromDgraph(ctx, userId, groupingId, startDate, endDate, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphDmRecordingListFromDgraph Failed to get chat recording list from dgraph err: %+v",
			err)

		return
	}

	if dgraphDm == nil {
		return
	}

	recordingsPagination.ParticipantIsMember = int(dgraphDm.ParticipantIsMember)

	if len(dgraphDm.Recordings) > pageSize {
		recordingsPagination.Recordings = dgraphDm.Recordings[:pageSize]
	} else {
		recordingsPagination.Recordings = dgraphDm.Recordings
	}

	recordingsPagination.HasMore = len(dgraphDm.Recordings) > pageSize

	return
}

func GetDgraphNewGroupChatIncludingChatFromDgraph(ctx context.Context, lastChatTime time.Time, groupingId string) (chats ChatPagination, err error) {
	dgraphChats, err := domain.GetDgraphNewChatIncludingChatFromDgraph(ctx, groupingId, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewGroupChatIncludingChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphNewChatIncludingChatFromDgraph(ctx context.Context, lastChatTime time.Time, fromUserId string, toUserId string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(fromUserId, toUserId)
	dgraphChats, err := domain.GetDgraphNewChatIncludingChatFromDgraph(ctx, groupingId, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewChatIncludingChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphLatestGroupChatFromDgraph(ctx context.Context, requestedUserUUID uuid.UUID, groupingId string) (chats ChatPagination, err error) {

	dgraphChats, err := domain.GetDgraphLatestChatFromDgraph(ctx, groupingId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphLatestGroupChatFromDgraph Failed to get chats from dgraph err: %+v",
			err)

		return
	}

	currentTime := time.Now()

	err = lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, groupingId, requestedUserUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphLatestGroupChatFromDgraph Failed to update chat last seen err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func GetDgraphLatestChatFromDgraph(ctx context.Context, requestedUserUUID uuid.UUID, otherUserUUID string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(requestedUserUUID.String(), otherUserUUID)

	dgraphChats, err := domain.GetDgraphLatestChatFromDgraph(ctx, groupingId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphLatestChatFromDgraph Failed to get chats from dgraph err: %+v",
			err)

		return
	}

	currentTime := time.Now()

	err = lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, groupingId, requestedUserUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphLatestChatFromDgraph Failed to update chat last seen err: %+v",
			err)

		return
	}

	if len(dgraphChats) > domain.CHAT_COUNT {
		chats.Chats = dgraphChats[:domain.CHAT_COUNT]
	} else {
		chats.Chats = dgraphChats
	}

	chats.HasMore = len(dgraphChats) > domain.CHAT_COUNT

	return
}

func CreateOrUpdateChatReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForChat, chatDgraph *dgraphStruct.DgraphChat, userDgraph *dgraphStruct.DgraphUser, grpId string) (reactionUID string, err error) {

	currentTime := time.Now()

	dgraphChat := &dgraphStruct.DgraphChat{
		Uid:   "uid(cha)",
		Uuid:  reactionInfo.Uuid,
		DType: []string{"Chat"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiReactionUuid,
				AddedAt:   &currentTime,
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: chatDgraph.From.Uid,
				},
				AddedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraph.Uid,
				},
			},
		},
	}

	reactionUID, err = domain.CreateOrUpdateChatReaction(ctx, dgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateChatReaction Failed to create/update reaction in chat err: %+v",
			err)

		return
	}

	mqttChatReaction := mqttStruct.MqttChatReaction{
		Type:            mqttStruct.TYPE_CREATE,
		EmojiReactionId: reactionInfo.EmojiReactionUuid,
		ChatUuid:        reactionInfo.Uuid,
		AddedByUuid:     userDgraph.Uuid,
		AddedByUserName: userDgraph.DisplayName(),
		ChatGrpId:       grpId,
		ReactionUuid:    reactionUID,
	}

	if len(mqttChatReaction.ReactionUuid) == 0 {
		mqttChatReaction.Type = mqttStruct.TYPE_UPDATE
		mqttChatReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	go mqttBusiness.PublishChatReaction(&mqttChatReaction, grpId)

	chatSnippet := chatDgraph.Body
	if len(chatSnippet) > 30 {
		chatSnippet = helpers.TruncateRunesWithSuffix(chatSnippet, 30, "...")
	}

	titlePush := fmt.Sprintf("%s reacted", userDgraph.DisplayName())
	bodyPush := fmt.Sprintf("Reacted to: \"%s\"", chatSnippet)

	go sendNewChatReactionNotification(titlePush, bodyPush, grpId, chatDgraph, userDgraph, reactionInfo.EmojiReactionUuid)

	return
}

func sendNewChatReactionNotification(title string, body string, grpId string, chatDgraph *dgraphStruct.DgraphChat, userDgraph *dgraphStruct.DgraphUser, reactionID string) {

	if chatDgraph.From.Uuid == userDgraph.Uuid {
		return
	}

	ctx := context.Background()

	activityItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_REACTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
		Time:         time.Now().Format(time.RFC3339),
	}

	activityBusiness.PublishActivityToUser(chatDgraph.From.Uuid, activityItem)

	var eligibleUserIDs []string

	eligibleUserIDs = append(eligibleUserIDs, userDgraph.Uuid)

	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatReactionNotification Failed to get user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = chatDgraph.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.DisplayName()
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)
	pushData[firebaseInit.FIREBASE_PUSH_DATA_REACTION_ID] = reactionID

	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendNewChatReactionNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

}

func DeleteChatReaction(ctx context.Context, chatDgraphUUID string, reactionDgraphUUID string, userDgraph *dgraphStruct.DgraphUser, grpId string, chatUUID string) (err error) {
	err = domain.DeleteChatReaction(ctx, chatDgraphUUID, reactionDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateChatReaction Failed to create/update reaction in chat err: %+v",
			err)

		return
	}

	mqttChatReaction := mqttStruct.MqttChatReaction{
		Type:         mqttStruct.TYPE_DELETE,
		ChatUuid:     chatUUID,
		AddedByUuid:  userDgraph.Uuid,
		ChatGrpId:    grpId,
		ReactionUuid: reactionDgraphUUID,
	}

	go mqttBusiness.PublishChatReaction(&mqttChatReaction, grpId)

	return
}

func GetDgraphChatByUUIDWithAllComments(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	dgraphChat, err = domain.GetDgraphChatByUUIDWithAllComments(ctx, chatUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateChatReaction Failed to create/update reaction in chat err: %+v",
			err)

		return
	}
	return
}

func CreateChatComment(ctx context.Context, commentInfo *adapter.InputCreateOrUpdateCommentToChat, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, grpId string, dgraphUserUid string, chatDgraphInfo *dgraphStruct.DgraphChat) (createdCommentData *adapter.OutputCreateChatCommentForPost, err error) {

	commentUUID := uuid.New()
	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	for _, mediaObj := range commentInfo.MediaObj {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphChat := dgraphStruct.DgraphChat{
		Uid:  "uid(cha)",
		Uuid: commentInfo.ChatUuid,
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  commentUUID.String(),
				Text:  commentInfo.HTMLText,
				Chat: &dgraphStruct.DgraphChat{
					Uid: "uid(cha)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: dgraphUserUid,
				},
				ChatGroupingId: grpId,
				Attachments:    commentInfo.MediaObj,
				CreatedAt:      &currentTime,
				DeletedAt:      &zeroUnixTime,
				Mentions: &dgraphStruct.DgraphMentions{
					Mentions:    commentMentions,
					CommentUuid: commentUUID.String(),
					Comment: &dgraphStruct.DgraphComment{
						Uid: "uid(co)",
					},
					CreatedAt: &currentTime,
				},
				CommentBy: &dgraphStruct.DgraphUser{
					Uid: userInfo.UserDgraphInfo.Uid,
				},
			},
		},
	}

	_, err = commentBusiness.CreateCommentInChat(ctx, &dgraphChat, userInfo, commentUUID, currentTime, chatDgraphInfo, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePostComment Failed to create comment on post err: %+v",
			err)
		return
	}

	createdCommentData = &adapter.OutputCreateChatCommentForPost{
		Uuid:             commentUUID.String(),
		CommentCreatedAt: currentTime,
	}

	mqttChatComment := mqttStruct.MqttChatComment{
		Type:           mqttStruct.TYPE_CREATE,
		ChatUuid:       commentInfo.ChatUuid,
		ChatGrpId:      grpId,
		ChatCreatedAt:  &currentTime,
		HTMLText:       commentInfo.HTMLText,
		UserUuid:       userInfo.UserDgraphInfo.Uuid,
		UserName:       userInfo.UserDgraphInfo.DisplayName(),
		UserProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		CommentUuid:    commentUUID.String(),
		Attachments:    commentInfo.MediaObj,
		IsBot:          userInfo.UserDgraphInfo.IsBot,
	}

	if !helpers.IsBulkImport(ctx) {
		go mqttBusiness.PublishChatComment(&mqttChatComment, grpId)

		go sendNewChatCommentActivity(currentTime, commentUUID.String(), commentInfo.HTMLText, &userInfo.UserDgraphInfo, grpId, mentionsDgraphUsersList, chatDgraphInfo)

		// Notify listeners so an AI teammate can CONTINUE a group-chat thread it
		// is part of: a person can @mention the agent again or answer its
		// needs_human question right in the thread and it picks the work back up,
		// replying as another comment on the SAME message. Gated to HUMAN
		// authors (an agent's own reply comment has IsBot=true) so the agent's
		// reply never re-triggers itself — the loop guard for chat threads.
		if !userInfo.UserDgraphInfo.IsBot {
			mentionIDs := make([]string, 0, len(mentionsDgraphUsersList))
			for _, m := range mentionsDgraphUsersList {
				if m != nil && m.Uid != "" {
					mentionIDs = append(mentionIDs, m.Uid)
				}
			}
			go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.comment.created", map[string]interface{}{
				"message_id":   commentInfo.ChatUuid,
				"group_id":     grpId,
				"sender_id":    userInfo.UserDgraphInfo.Uuid,
				"author_name":  userInfo.UserDgraphInfo.DisplayName(),
				"comment_uuid": commentUUID.String(),
				"text":         helpers.HTMLToPlainText(commentInfo.HTMLText),
				"mention_ids":  mentionIDs,
			})
		}
	}

	return
}

// PostChatCommentAsBot posts an in-thread reply — a comment on an existing chat
// message — authored by a bot principal (the AI coworker or a DM-able agent).
// This is the Slack-style "reply in thread" path for chats/group chats: the AI
// answers as a threaded comment on the message that summoned it rather than a
// new message in the conversation.
//
// It reuses the exact shared comment core (CreateChatComment ->
// commentBusiness.CreateCommentInChat), so a bot comment gets the same Postgres
// row + Dgraph node + OpenSearch index + AI embedding + live MQTT +
// participant notifications as a human one, staying perfectly consistent and
// generic across every surface. Loop-safe: CreateChatComment emits MQTT +
// activity but NO workspace event, so it can never re-trigger the coworker or
// an agent.
//
// botInfo is the bot's resolved UserInfo (its own principal); chatUUID is the
// triggering message. A missing/deleted chat message is a hard error so the
// caller can fall back to a plain reply.
func PostChatCommentAsBot(ctx context.Context, botInfo *userModels.UserInfo, chatUUID, htmlText string) (*adapter.OutputCreateChatCommentForPost, error) {
	if botInfo == nil {
		return nil, fmt.Errorf("bot identity is not available")
	}
	if strings.TrimSpace(htmlText) == "" {
		return nil, fmt.Errorf("text is required")
	}
	dgraphChat, err := GetDgraphChatBasicByUUID(ctx, chatUUID, botInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChat == nil || dgraphChat.From == nil || dgraphChat.DM == nil {
		if err == nil {
			err = fmt.Errorf("chat message not found")
		}
		return nil, err
	}
	commentInfo := &adapter.InputCreateOrUpdateCommentToChat{
		ChatUuid: chatUUID,
		HTMLText: htmlText,
	}
	// ContentAddedBy is the original message author (dgraphChat.From.Uid);
	// CommentBy is the bot (carried by botInfo inside CreateChatComment).
	return CreateChatComment(ctx, commentInfo, botInfo, nil, dgraphChat.DM.GroupingId, dgraphChat.From.Uid, dgraphChat)
}

// EditChatCommentAsBot edits an EXISTING in-thread chat comment authored by a
// bot principal, replacing its body with htmlText — the chat/DM analog of
// botpost.EditCommentToPostAsBot (async-mentions spec Task 1.2). It reuses the
// loop-safe UpdateChatComment path (PG + Dgraph + OpenSearch + MQTT TYPE_UPDATE,
// no workspace event), so an edit can never re-trigger an agent. The context is
// workflow-tagged defensively. Best-effort caller contract: on any error the
// caller falls back to a fresh comment.
func EditChatCommentAsBot(ctx context.Context, botInfo *userModels.UserInfo, chatUUID string, commentUUID uuid.UUID, htmlText string) error {
	if botInfo == nil {
		return fmt.Errorf("bot identity is not available")
	}
	if strings.TrimSpace(htmlText) == "" {
		return fmt.Errorf("text is required")
	}
	if strings.TrimSpace(chatUUID) == "" || commentUUID == uuid.Nil {
		return fmt.Errorf("chat and comment ids are required")
	}

	dgraphChat, err := GetDgraphChatBasicByUUID(ctx, chatUUID, botInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChat == nil || dgraphChat.DM == nil {
		if err == nil {
			err = fmt.Errorf("chat message not found")
		}
		return err
	}

	// Never let a bot edit re-trigger workflows/agents (the update path already
	// emits no workspace event; this is belt-and-suspenders).
	ctx = helpers.WithWorkflowGenerated(ctx)

	commentInfo := &adapter.InputCreateOrUpdateCommentToChat{
		ChatUuid: chatUUID,
		Uuid:     commentUUID.String(),
		HTMLText: htmlText,
	}
	// Minimal raw comment carrying the author (bot) for the MQTT edit event.
	rawComment := &dgraphStruct.DgraphComment{
		CommentBy: &dgraphStruct.DgraphUser{Uuid: botInfo.UserDgraphInfo.Uuid},
	}
	return UpdateChatComment(ctx, commentUUID, commentInfo, nil, dgraphChat, rawComment)
}

func sendNewChatCommentActivity(currentTime time.Time, commentUUID string, commentBody string, userDgraph *dgraphStruct.DgraphUser, grpId string, mentions []*dgraphStruct.DgraphUser, chatDgraph *dgraphStruct.DgraphChat) {
	ctx := context.Background()

	var mentionUUDs []string

	for _, m := range mentions {
		mentionUUDs = append(mentionUUDs, m.Uuid)
	}

	// 1. Get eligible users for notifications (based on preferences and grouping)
	eligibleUserIDs, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grpId, userDgraph.Uuid, mentionUUDs, true)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatCommentActivity Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Publish MQTT Activity Events
	for _, recipientID := range eligibleUserIDs {
		// Create a simplified activity item for MQTT
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid: commentUUID,
				Text: commentBody,
				CommentBy: &dgraphStruct.DgraphUser{
					Uuid:     userDgraph.Uuid,
					UserName: userDgraph.DisplayName(),
				},
				Chat: &dgraphStruct.DgraphChat{
					Uuid: chatDgraph.Uuid,
				},
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(recipientID, activityItem)
	}

	if userDgraph.Uuid != chatDgraph.From.Uuid {

		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT, // In chats, it's either a mention or a new message (which we'll treat as a notification)
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid: commentUUID,
				Text: commentBody,
				CommentBy: &dgraphStruct.DgraphUser{
					Uuid:     userDgraph.Uuid,
					UserName: userDgraph.DisplayName(),
				},
				Chat: &dgraphStruct.DgraphChat{
					Uuid: chatDgraph.Uuid,
				},
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(userDgraph.Uuid, activityItem)

	}

	// Email fan-out for chat comment.
	// Recipients: mentioned users + (if not self) the original chat author.
	emailRecipients := append([]string{}, eligibleUserIDs...)
	if chatDgraph.From != nil && chatDgraph.From.Uuid != "" && chatDgraph.From.Uuid != userDgraph.Uuid {
		emailRecipients = append(emailRecipients, chatDgraph.From.Uuid)
	}
	notificationBusiness.DispatchChatComment(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		grpId,
		chatDgraph.Uuid,
		helpers.HTMLToPlainText(commentBody),
		commentUUID,
		emailRecipients,
	)

}

func UpdateChatComment(ctx context.Context, commentUUID uuid.UUID, commentInfo *adapter.InputCreateOrUpdateCommentToChat, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, rawDgraphChat *dgraphStruct.DgraphChat, rawDgraphComment *dgraphStruct.DgraphComment) (err error) {
	currentTime := time.Now()

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	dgraphComment := dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  commentInfo.Uuid,
		DType: []string{"Comment"},
		Text:  commentInfo.HTMLText,
		// Attachments: commentInfo.MediaObj,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid:      "uid(me)",
			Mentions: commentMentions,
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
			CommentUuid: commentInfo.Uuid,
			UpdatedAt:   &currentTime,
		},
		UpdatedAt: &currentTime,
	}

	err = commentBusiness.UpdateComment(ctx, &dgraphComment, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChatComment Failed to update comment on chat err: %+v",
			err)
		return
	}

	mqttChatComment := mqttStruct.MqttChatComment{
		Type:        mqttStruct.TYPE_UPDATE,
		ChatUuid:    rawDgraphChat.Uuid,
		ChatGrpId:   rawDgraphChat.DM.GroupingId,
		CommentUuid: commentUUID.String(),
		UserUuid:    rawDgraphComment.CommentBy.Uuid,
		HTMLText:    commentInfo.HTMLText,
	}

	if helpers.IsBulkImport(ctx) {
		// Skip the live MQTT chat-comment edit during bulk Slack
		// import; the Dgraph + Postgres update above is enough.
		return
	}

	go mqttBusiness.PublishChatComment(&mqttChatComment, rawDgraphChat.DM.GroupingId)

	return
}

func DeleteCommentOnChat(ctx context.Context, commentUUID uuid.UUID, userUUID string, chatId string, rawDgraphComment *dgraphStruct.DgraphComment) (err error) {

	currentTime := time.Now()
	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:       "uid(co)",
		Uuid:      commentUUID.String(),
		DeletedAt: &currentTime,
		DType:     []string{"Comment"},
	}
	err = commentBusiness.DeleteComment(ctx, dgraphComment, currentTime, commentUUID, rawDgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteCommentOnChat Failed to delete comment from chat err: %+v",
			err)

		return
	}

	mqttChatComment := mqttStruct.MqttChatComment{
		Type:        mqttStruct.TYPE_DELETE,
		ChatUuid:    chatId,
		ChatGrpId:   rawDgraphComment.ChatGroupingId,
		CommentUuid: commentUUID.String(),
		UserUuid:    rawDgraphComment.Uuid,
	}

	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishChatComment(&mqttChatComment, rawDgraphComment.ChatGroupingId)

	return
}

func CreateOrUpdateChatCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInChat, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	currentTime := time.Now()

	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  reactionInfo.Uuid,
		DType: []string{"Comment"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiReactionUuid,
				AddedAt:   &currentTime,
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: dgraphCommentRaw.CommentBy.Uid,
				},
				AddedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraph.Uid,
				},
			},
		},
	}

	reactionUUID, err = commentBusiness.CreateOrUpdateDgraphCommentReaction(ctx, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateChatCommentReaction Failed to update comment reaction in chat err: %+v",
			err)
		return
	}

	mqttChatCommentReaction := mqttStruct.MqttChatCommentReaction{
		Type:            mqttStruct.TYPE_CREATE,
		EmojiReactionId: reactionInfo.EmojiReactionUuid,
		CommentUuid:     reactionInfo.Uuid,
		AddedByUserName: userDgraph.DisplayName(),
		AddedByUuid:     userDgraph.Uuid,
		ReactionUuid:    reactionUUID,
		ChatUuid:        dgraphCommentRaw.Chat.Uuid,
	}

	if len(mqttChatCommentReaction.ReactionUuid) == 0 {
		mqttChatCommentReaction.Type = mqttStruct.TYPE_UPDATE
		mqttChatCommentReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	// Suppress MQTT + push notification side-effects during a bulk
	// Slack import. The Dgraph reaction node is already written above
	// so the data lands; only the live UI/notification fan-out is
	// skipped to avoid alerting every participant about historical
	// reactions.
	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishChatCommentReaction(&mqttChatCommentReaction, helpers.GetGroupingId(dgraphCommentRaw.Chat.From.Uuid, dgraphCommentRaw.Chat.DM.GroupingId))

	commentSnippet := dgraphCommentRaw.Text
	if len(commentSnippet) > 30 {
		commentSnippet = helpers.TruncateRunesWithSuffix(commentSnippet, 30, "...")
	}

	pushTitlle := fmt.Sprintf("%s reacted", userDgraph.DisplayName())
	pushBody := fmt.Sprintf("Reacted to: \"%s\"", commentSnippet)
	go sendNewChatCommentReactionNotification(pushTitlle, pushBody, dgraphCommentRaw, userDgraph, reactionInfo.EmojiReactionUuid)

	return
}

func sendNewChatCommentReactionNotification(title string, body string, commentDgraph *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser, reactionID string) {

	if commentDgraph.CommentBy.Uuid == userDgraph.Uuid {
		return
	}

	ctx := context.Background()

	activityItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_REACTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
		Time:         time.Now().Format(time.RFC3339),
	}

	activityBusiness.PublishActivityToUser(commentDgraph.CommentBy.Uuid, activityItem)

	var eligibleUserIDs []string

	eligibleUserIDs = append(eligibleUserIDs, userDgraph.Uuid)

	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatCommentReactionNotification Failed to get user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT_REACTION
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = commentDgraph.ChatGroupingId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = commentDgraph.Chat.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.DisplayName()
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)
	pushData[firebaseInit.FIREBASE_PUSH_DATA_REACTION_ID] = reactionID

	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendNewChatCommentReactionNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

}

func DeleteReactionOnCommentChat(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionDgraph *dgraphStruct.DgraphReaction) (err error) {
	err = commentBusiness.DeleteCommentReaction(ctx, commentDgraph.Uid, reactionDgraph.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteReactionOnCommentChat Failed to delete reaction on comment in post err: %+v",
			err)

		return
	}

	mqttChatCommentReaction := mqttStruct.MqttChatCommentReaction{
		Type:         mqttStruct.TYPE_DELETE,
		CommentUuid:  commentDgraph.Uuid,
		ReactionUuid: reactionDgraph.Uid,
		ChatUuid:     commentDgraph.Chat.Uuid,
		AddedByUuid:  reactionDgraph.AddedBy.Uuid,
	}

	go mqttBusiness.PublishChatCommentReaction(&mqttChatCommentReaction, helpers.GetGroupingId(commentDgraph.Chat.From.Uuid, commentDgraph.Chat.DM.GroupingId))

	return
}

func PublishChatTyping(userDgraph *dgraphStruct.DgraphUser, grpId string) {
	mqttChatTyping := mqttStruct.MqttChatTyping{
		UserUUID:  userDgraph.Uuid,
		UserName:  userDgraph.DisplayName(),
		ChatGrpId: grpId,
	}

	if userDgraph.ProfileKey != nil {
		mqttChatTyping.UserProfile = *userDgraph.ProfileKey
	}
	go mqttBusiness.PublishChatTyping(&mqttChatTyping, grpId)
}

func MakeVideoCall(ctx context.Context, grpId string, userDraphInfo *dgraphStruct.DgraphUser, isAdmin bool, audioEnabled bool, videoEnabled bool, isGroup bool) (token string, alreadyExisted bool, err error) {

	token, alreadyExisted, err = LiveKitBusiness.CreateRoomAndGetToken(ctx, grpId, userDraphInfo, isAdmin, audioEnabled, videoEnabled)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoCall Failed to make video call post err: %+v",
			err)

		return
	}

	if !alreadyExisted {

		pushTitle := fmt.Sprintf("Call from %s", userDraphInfo.DisplayName())

		body := "calling you..."

		if isGroup {
			pushTitle = fmt.Sprintf("%s started group call", userDraphInfo.DisplayName())

			body = "started call"

		}

		mqttChatCall := mqttStruct.MqttChatCall{
			CallActive: mqttStruct.MESSAGE_CALL_ACTIVE,
			GrpId:      grpId,
		}

		go mqttBusiness.PublishChatCall(&mqttChatCall, grpId)

		go sendChatCallNotification(pushTitle, body, userDraphInfo.Uuid, grpId, userDraphInfo.DisplayName(), userDraphInfo.ProfileKey)
	}

	return
}

func PublishStopChatCall(grpId string) {

	mqttChatCall := mqttStruct.MqttChatCall{
		CallActive: mqttStruct.MESSAGE_CALL_INACTIVE,
		GrpId:      grpId,
	}

	go mqttBusiness.PublishChatCall(&mqttChatCall, grpId)

}

func sendChatCallNotification(title string, body string, userId string, grpId string, username string, profileKey *string) {

	ctx := context.Background()

	var mentionUUDs []string

	// 1. Get eligible users for notifications (based on preferences and grouping)
	eligibleUserIDs, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grpId, userId, mentionUUDs, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendChatCallNotification Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Handle FCM Push Notifications
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendChatCallNotification Failed to get user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = username
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, profileKey)

	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendChatCallNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out for incoming chat call.
	notificationBusiness.DispatchChatCall(
		userId,
		username,
		userBusiness.GetSignedProfileURL(ctx, profileKey),
		grpId,
		eligibleUserIDs,
	)

}

func StartRecordingChatCall(ctx context.Context, userDgraphInfo *dgraphStruct.DgraphUser, actualGrpId string) (err error) {
	egresssInfo, filePath, err := LiveKitBusiness.StartRecording(ctx, actualGrpId, userDgraphInfo)

	zeroEpochTime := time.Time{}

	currentTime := time.Now()

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StartRecordingChatCall Failed to start call recording err: %+v",
			err)

		return
	}

	dgraphDmInfo := &dgraphStruct.DgraphDm{
		Uid: "uid(dm)",
		Recordings: []*dgraphStruct.DgraphRecording{

			{

				EgressId:  egresssInfo.EgressId,
				EndedAt:   &zeroEpochTime,
				StartedAt: &currentTime,
				ObjectKey: filePath,
				Dm: &dgraphStruct.DgraphDm{
					Uid: "uid(dm)",
				},
				RecordingStartedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraphInfo.Uid,
				},
			},
		},
	}

	_, _, err = domain.CreateDM(ctx, dgraphDmInfo, actualGrpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StartRecordingChatCall Failed to update chat DM err: %+v",
			err)

		return
	}
	return
}

func StopRecordingChatCall(ctx context.Context, grpId string) (err error) {
	err = LiveKitBusiness.StopRecording(ctx, grpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StopRecordingChatCall Failed to start call recording err: %+v",
			err)

		return
	}

	return
}

// GetDgraphDmBasicByGrpId returns basic DM info (including dm_is_member) for the given groupingId.
// Used to verify that the caller is a member of a group chat before forwarding a message to it.
func GetDgraphDmBasicByGrpId(ctx context.Context, grpId string, userDgraphUID string) (dgraphDm *dgraphStruct.DgraphDm, err error) {
	dgraphDm, err = domain.GetDgraphDmBasicInfoFromDgraph(ctx, userDgraphUID, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphDmBasicByGrpId Failed to get dm basic info err: %+v",
			err)
	}
	return
}

// ParticipatesInGrouping reports whether a person is a participant in a DM or group
// chat grouping.
//
// The authority rule for chat surfaces, in one place. A DM and a group chat differ
// only in how many participants a grouping has, so both are answered here rather
// than by two rules that would have to be kept in agreement.
//
// WHY THIS IS AN EXPLICIT CHECK AND NOT A FILTER. The in-app summarize path enforces
// access by passing the caller's accessible grouping ids into the search's permission
// filter, so a non-participant gets an empty result. That is safe, but it is not
// answerable: "no messages" and "not allowed" look identical, so a refusal cannot be
// stated to the caller or recorded as a refusal in the audit trail. An authorization
// layer needs a decision, not an absence.
//
// Returns false with no error for a grouping that does not exist, so a caller cannot
// distinguish "no such conversation" from "not yours" — which would otherwise be a
// way to enumerate groupings.
func ParticipatesInGrouping(ctx context.Context, groupingId string, userDgraphUID string) (bool, error) {
	groupingId = strings.TrimSpace(groupingId)
	userDgraphUID = strings.TrimSpace(userDgraphUID)
	if groupingId == "" || userDgraphUID == "" {
		return false, nil
	}

	dm, err := domain.GetDmParticipation(ctx, groupingId, userDgraphUID)
	if err != nil {
		return false, err
	}
	if dm == nil {
		return false, nil
	}
	return dm.ParticipantIsMember > 0, nil
}
