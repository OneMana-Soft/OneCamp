package business

import (
	"context"
	"fmt"
	"slices"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/User"
	lastseenChannelBusiness "github.com/akashc777/OneCamp/business/LastSeenChannel"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	domain "github.com/akashc777/OneCamp/domain/BulkPostAndChat"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/google/uuid"
)

func BulkPostAndChatForward(ctx context.Context, userInfo *dgraphStruct.DgraphUser, fwdMessageInput *adapter.UserFwdMsgInput, dgraphMentions []*dgraphStruct.DgraphUser, userDgraphPost *dgraphStruct.DgraphPost, userDgraphChat *dgraphStruct.DgraphChat) (err error) {

	currentTime := time.Now()

	var dgraphPosts []*dgraphStruct.DgraphPost
	var postCount = 0

	var dgraphDMs []*dgraphStruct.DgraphDm
	var dmCount = 0

	for i, _ := range fwdMessageInput.Attachments {
		fwdMessageInput.Attachments[i].DType = []string{"Attachment"}
	}

	var postUUIDs []string
	var channelUUIDs []string
	var channelNames []string

	var chatUUIDs []string
	var chatToUUIDs []string
	var chatGroupingUUIDs []string

	// Group chat destination tracking (separate from DM)
	var grpChatUUIDs []string
	var grpChatGroupingIDs []string

	// participantsByChatUUID keys each 1:1 forward's participants to the chat UUID they
	// belong to. Keyed rather than positional on purpose: FwdTo arrives in whatever order
	// the client picked destinations and freely interleaves channels, group chats and 1:1
	// DMs, so a slice index does NOT identify a destination. Presence in this map is also
	// what marks a forward as 1:1 downstream.
	participantsByChatUUID := map[string][]*openSearchStruct.OpenSearchChatParticipants{}

	for _, postOrChat := range fwdMessageInput.FwdTo {

		var chatParticipantsTemp []*openSearchStruct.OpenSearchChatParticipants
		if postOrChat.ChannelUuid != "" {

			postUUID := uuid.New()

			postUUIDs = append(postUUIDs, postUUID.String())
			channelUUIDs = append(channelUUIDs, postOrChat.ChannelUuid)
			channelNames = append(channelNames, postOrChat.ChannelName)

			channelPost := dgraphStruct.DgraphChannel{
				DType: []string{"Channel"},
				Uid:   postOrChat.ChannelDgraphUid,
				Posts: []*dgraphStruct.DgraphPost{
					{
						Uid:   fmt.Sprintf("uid(po_%d)", postCount),
						DType: []string{"Post"},
					},
				},
			}

			dgraphPost := &dgraphStruct.DgraphPost{
				Uid:      fmt.Sprintf("uid(po_%d)", postCount),
				Uuid:     postUUID.String(),
				DType:    []string{"Post"},
				Text:     fwdMessageInput.HtmlText,
				MediaObj: fwdMessageInput.Attachments,
				Mentions: &dgraphStruct.DgraphMentions{
					DType:     []string{"Mention"},
					Mentions:  dgraphMentions,
					CreatedAt: &currentTime,
					Post: &dgraphStruct.DgraphPost{
						Uid: fmt.Sprintf("uid(po_%d)", postCount),
					},
					PostUuid: postUUID.String(),
				},
				PostBy: &dgraphStruct.DgraphUser{
					DType: []string{"User"},
					Uid:   userInfo.Uid,
					Posts: []*dgraphStruct.DgraphPost{
						{
							Uid:   fmt.Sprintf("uid(po_%d)", postCount),
							DType: []string{"Post"},
						},
					},
				},
				Channel:   &channelPost,
				CreatedAt: &currentTime,
			}

			if userDgraphPost != nil {

				dgraphPost.ForwaredPost = &dgraphStruct.DgraphPost{
					Uid: userDgraphPost.Uid,
				}
			}

			if userDgraphChat != nil {
				dgraphPost.ForwarededChat = &dgraphStruct.DgraphChat{
					Uid: userDgraphChat.Uid,
				}
			}

			dgraphPosts = append(dgraphPosts, dgraphPost)
			postCount = postCount + 1

		} else if postOrChat.GrpId != "" {

			// ── Forward to an EXISTING group chat ──
			chatUUID := uuid.New()
			grpChatUUIDs = append(grpChatUUIDs, chatUUID.String())
			grpChatGroupingIDs = append(grpChatGroupingIDs, postOrChat.GrpId)

			// The DM already exists — we just add a new chat node under it.
			dgraphGrpDm := &dgraphStruct.DgraphDm{
				Uid:        postOrChat.GrpDgraphUid, // existing DM node UID
				GroupingId: postOrChat.GrpId,
				DType:      []string{"Dm"},
				Chats: []*dgraphStruct.DgraphChat{
					{
						Uid:       fmt.Sprintf("uid(grpch_%d)", dmCount),
						Uuid:      chatUUID.String(),
						DType:     []string{"Chat"},
						CreatedAt: &currentTime,
						From: &dgraphStruct.DgraphUser{
							Uid: userInfo.Uid,
						},
						MediaObj: fwdMessageInput.Attachments,
						Body:     fwdMessageInput.HtmlText,
						Mentions: &dgraphStruct.DgraphMentions{
							Mentions:  dgraphMentions,
							ChatUuid:  chatUUID.String(),
							CreatedAt: &currentTime,
							Chat: &dgraphStruct.DgraphChat{
								Uid: fmt.Sprintf("uid(grpch_%d)", dmCount),
							},
						},
					},
				},
			}

			if userDgraphPost != nil {
				dgraphGrpDm.Chats[0].FwdMsgPost = &dgraphStruct.DgraphPost{
					Uid: userDgraphPost.Uid,
				}
			}

			if userDgraphChat != nil {
				dgraphGrpDm.Chats[0].FwdMsgChat = &dgraphStruct.DgraphChat{
					Uid: userDgraphChat.Uid,
				}
			}

			dgraphDMs = append(dgraphDMs, dgraphGrpDm)
			dmCount = dmCount + 1

		} else if postOrChat.UserUuid != "" {

			// ── Forward to a 1:1 DM ──
			chatParticipantsTemp = append(chatParticipantsTemp, &openSearchStruct.OpenSearchChatParticipants{
				Uuid:       userInfo.Uuid,
				Name:       userInfo.UserName,
				ProfileKey: userInfo.ProfileKey,
			})
			chatParticipantsTemp = append(chatParticipantsTemp, &openSearchStruct.OpenSearchChatParticipants{
				Uuid:       postOrChat.UserUuid,
				Name:       postOrChat.UserName,
				ProfileKey: &postOrChat.UserProfileKey,
			})

			chatUUID := uuid.New()
			groupingId := helpers.GetGroupingId(userInfo.Uuid, postOrChat.UserUuid)

			chatGroupingUUIDs = append(chatGroupingUUIDs, groupingId)
			chatUUIDs = append(chatUUIDs, chatUUID.String())
			chatToUUIDs = append(chatToUUIDs, postOrChat.UserUuid)

			dgraphDm := &dgraphStruct.DgraphDm{
				Uid:        fmt.Sprintf("uid(dm_%d)", dmCount),
				GroupingId: groupingId,
				DType:      []string{"Dm"},
				// Its two people, as a DM started any other way has: every
				// check of who's in it reads them, so a DM a forward started
				// refused replies, reactions and receipts until someone wrote
				// in it.
				Participants: []*dgraphStruct.DgraphUser{{Uid: userInfo.Uid}, {Uid: postOrChat.UserDgraphUid}},
				Chats: []*dgraphStruct.DgraphChat{
					{
						Uid:       fmt.Sprintf("uid(ch_%d)", dmCount),
						Uuid:      chatUUID.String(),
						DType:     []string{"Chat"},
						CreatedAt: &currentTime,
						To: &dgraphStruct.DgraphUser{
							Uid: postOrChat.UserDgraphUid,
							DMs: []*dgraphStruct.DgraphDm{
								{
									Uid: fmt.Sprintf("uid(dm_%d)", dmCount),
								},
							},
						},
						From: &dgraphStruct.DgraphUser{
							Uid: userInfo.Uid,
							DMs: []*dgraphStruct.DgraphDm{
								{
									Uid: fmt.Sprintf("uid(dm_%d)", dmCount),
								},
							},
						},
						MediaObj: fwdMessageInput.Attachments,
						Body:     fwdMessageInput.HtmlText,
						Mentions: &dgraphStruct.DgraphMentions{
							Mentions:  dgraphMentions,
							ChatUuid:  chatUUID.String(),
							CreatedAt: &currentTime,
							Chat: &dgraphStruct.DgraphChat{
								Uid: fmt.Sprintf("uid(ch_%d)", dmCount),
							},
						},
					},
				},
			}

			if userDgraphPost != nil {
				dgraphDm.Chats[0].FwdMsgPost = &dgraphStruct.DgraphPost{
					Uid: userDgraphPost.Uid,
				}
			}

			if userDgraphChat != nil {
				dgraphDm.Chats[0].FwdMsgChat = &dgraphStruct.DgraphChat{
					Uid: userDgraphChat.Uid,
				}
			}

			dgraphDMs = append(dgraphDMs, dgraphDm)
			dmCount = dmCount + 1 // Bug 5 fix: single increment per iteration (removed duplicate)

			participantsByChatUUID[chatUUID.String()] = chatParticipantsTemp
		}
	}

	if len(postUUIDs) == 0 && len(channelUUIDs) == 0 && len(chatUUIDs) == 0 && len(chatToUUIDs) == 0 && len(chatGroupingUUIDs) == 0 && len(grpChatUUIDs) == 0 {

		return
	}

	// slices.Concat, not append(a, b...): appending to an accumulated slice writes into ITS
	// backing array whenever capacity allows, so the result aliases a slice this function
	// still uses and hands to goroutines below. No live corruption today — the values written
	// happen to be identical — but this is a fragile way to combine 1:1 and group
	// destinations in a function whose destination bookkeeping already went wrong once.
	// Concat always allocates, so each combined list is independent.
	//
	// Both concatenations put 1:1 destinations before group ones, which is what keeps
	// allChatUUIDs and allChatGrpIDs positionally aligned with each other.
	allChatUUIDs := slices.Concat(chatUUIDs, grpChatUUIDs)
	allChatGrpIDs := slices.Concat(chatGroupingUUIDs, grpChatGroupingIDs)

	err = domain.BulkAddChatAndPostToPostgres(postUUIDs, channelUUIDs, allChatUUIDs, allChatGrpIDs, userInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkPostAndChatForward Failed to bulk add post and chat to postgres err: %+v",
			err,
		)
		return
	}

	_, err = domain.BulkAddChatAndPostToDgraph(ctx, dgraphPosts, dgraphDMs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkPostAndChatForward Failed to bulk add post and chat to dgraph err: %+v",
			err,
		)
		// The rows written above name posts and chats the graph doesn't
		// have, and nothing would ever show or remove them: they go too.
		_ = helpers.CompensateOnFailure(ctx, "forwarded posts and chats", func(c context.Context) error {
			return domain.BulkRemoveChatAndPostFromPostgres(c, postUUIDs, allChatUUIDs)
		})
		return
	}

	senderUUID, parseErr := uuid.Parse(userInfo.Uuid)
	if parseErr == nil {
		// Bug 1 Fix: Update sender's last_seen_chat for all forwards (DM + GrpChats) in one DB call
		allGrpIds := slices.Concat(chatGroupingUUIDs, grpChatGroupingIDs)
		_ = lastseenBusiness.BulkUpdateLastSeenChatForSender(ctx, allGrpIds, userInfo.Uuid, currentTime)

		// Mirrors postController.go:125 — update sender's last_seen_channel for each channel forward in bulk
		var parsedChUUIDs []uuid.UUID
		for _, chUUID := range channelUUIDs {
			if parsedChUUID, parseChErr := uuid.Parse(chUUID); parseChErr == nil {
				parsedChUUIDs = append(parsedChUUIDs, parsedChUUID)
			}
		}
		_ = lastseenChannelBusiness.BulkUpdateLastSeenChannelForUser(ctx, senderUUID, parsedChUUIDs, currentTime)
	}

	// Bug 1 Fix: Create notification records for 1:1 DM forward recipients in bulk
	chatNotificationBusiness.BulkCreateDMNotificationsIfNotExists(
		ctx,
		userInfo.Uuid,
		chatToUUIDs,
		chatGroupingUUIDs,
		postgressStruct.NOTIFICATION_TYPE_ALL,
		currentTime,
	)

	// add to opensearch
	AddChatsAndPostsToOpensearch(dgraphPosts, dgraphDMs, fwdMessageInput.HtmlText, channelUUIDs, channelNames, userInfo, participantsByChatUUID)

	// publish via MQTT for real-time delivery
	go PublishChatsAndPostsMqtt(dgraphPosts, dgraphDMs, fwdMessageInput.HtmlText, channelUUIDs, userInfo, userDgraphPost, userDgraphChat)

	// Bug 1 Fix: Send push notifications for DM and group chat forwards
	go sendFwdMessageNotifications(ctx, userInfo, fwdMessageInput.HtmlText, chatGroupingUUIDs, grpChatGroupingIDs)

	return

}

// sendFwdMessageNotifications sends FCM push notifications for forwarded messages to DMs and group chats.
// Mirrors the sendNewChatNotification pattern in chatBusiness.go.
func sendFwdMessageNotifications(ctx context.Context, userInfo *dgraphStruct.DgraphUser, htmlText string, dmGroupingIDs []string, grpGroupingIDs []string) {

	plainText := helpers.HTMLToPlainText(htmlText)
	if len(plainText) == 0 {
		plainText = "Forwarded a message"
	}

	// slices.Concat, not append: this runs in a goroutine, and appending would write into the
	// caller's backing array while the caller may still be using it.
	allGroupingIDs := slices.Concat(dmGroupingIDs, grpGroupingIDs)

	for _, grpId := range allGroupingIDs {
		eligibleUserIDs, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grpId, userInfo.Uuid, nil, false)
		if err != nil || len(eligibleUserIDs) == 0 {
			continue
		}

		tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)
		if err != nil || len(tokens) == 0 {
			continue
		}

		pushData := make(map[string]string)
		pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT
		pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
		pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = userInfo.UserName
		pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = plainText
		pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userInfo.UserName
		pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userInfo.ProfileKey)

		batchSize := 500
		for i := 0; i < len(tokens); i += batchSize {
			end := i + batchSize
			if end > len(tokens) {
				end = len(tokens)
			}
			_ = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokens[i:end])
		}
	}
}

func PublishChatsAndPostsMqtt(dgraphPosts []*dgraphStruct.DgraphPost,
	dgraphDMs []*dgraphStruct.DgraphDm, HTMLText string,
	channelUUIDs []string, userInfo *dgraphStruct.DgraphUser,
	userDgraphPost *dgraphStruct.DgraphPost, userDgraphChat *dgraphStruct.DgraphChat) {

	for i, dgraphPost := range dgraphPosts {

		mqttCreatePost := mqttStruct.MqttPost{
			Type:             mqttStruct.TYPE_CREATE,
			PostAttachments:  dgraphPost.MediaObj,
			PostHtmlText:     HTMLText,
			PostCreatedAt:    dgraphPost.CreatedAt,
			PostByUserUuid:   userInfo.Uuid,
			PostByProfileKey: userInfo.ProfileKey,
			PostByUserName:   userInfo.UserName,
			PostChannelUuid:  channelUUIDs[i],
			PostFwdPost:      userDgraphPost,
			PostFwdChat:      userDgraphChat,
			PostUuid:         dgraphPost.Uuid,
		}

		go mqttBusiness.PublishPost(&mqttCreatePost, channelUUIDs[i])

	}

	for _, dgraphDm := range dgraphDMs {

		if len(dgraphDm.Chats) == 0 {
			continue
		}
		dgraphChat := dgraphDm.Chats[0]

		// The node carries its OWN grouping id — set for both a 1:1 DM and an existing group
		// chat when it was built. It is read directly and never overridden by slice position:
		// this loop walks every forwarded DM while the old groupingUUIDs slice only grew for
		// 1:1 destinations, so `groupingUUIDs[i]` could hand a group chat's message the
		// grouping id of an unrelated 1:1 conversation and publish it to that topic.
		grpId := dgraphDm.GroupingId

		mqttChat := mqttStruct.MqttChat{
			Type:             mqttStruct.TYPE_CREATE,
			ChatUuid:         dgraphChat.Uuid,
			ChatCreatedAt:    dgraphChat.CreatedAt,
			ChatByProfileKey: userInfo.ProfileKey,
			ChatByUserName:   userInfo.UserName,
			ChatByUserUuid:   userInfo.Uuid,
			ChatHtmlText:     HTMLText,
			ChatAttachments:  dgraphChat.MediaObj,
			ChatGrpId:        grpId,
			ChatFwdPost:      userDgraphPost,
			ChatFwdChat:      userDgraphChat,
		}

		go mqttBusiness.PublishChat(&mqttChat, grpId)

	}

}

// forwardedDMTarget is one 1:1 DM destination of a forwarded message, already paired with
// the grouping id and participants that belong to IT.
type forwardedDMTarget struct {
	Chat         *dgraphStruct.DgraphChat
	GroupingID   string
	Participants []*openSearchStruct.OpenSearchChatParticipants
}

// forwardedDMTargets pairs each forwarded DM node with its own grouping id and participants,
// dropping group-chat forwards (which are indexed by a separate path).
//
// Pairing is by chat UUID. It used to be by slice position, and that was wrong: dgraphDMs
// grows for BOTH group and 1:1 destinations while the participants and grouping-id slices
// only grew for 1:1 ones, and FwdTo is in client-chosen order. Forwarding a single message
// to a group chat and a DM at once was therefore enough to index the group's message under
// the DM's grouping id and participant list — putting it in front of people who were never
// in that group — while silently not indexing the DM forward at all.
//
// A DM with no participants entry is a group forward, which is precisely the "only index 1:1
// DM forwards" rule the positional guard was reaching for.
//
// Pure, so the pairing can be tested without Dgraph, OpenSearch or MQTT.
func forwardedDMTargets(
	dgraphDMs []*dgraphStruct.DgraphDm,
	participantsByChatUUID map[string][]*openSearchStruct.OpenSearchChatParticipants,
) []forwardedDMTarget {
	targets := make([]forwardedDMTarget, 0, len(dgraphDMs))
	for _, dm := range dgraphDMs {
		if dm == nil || len(dm.Chats) == 0 || dm.Chats[0] == nil {
			continue
		}
		chat := dm.Chats[0]
		participants, isOneToOne := participantsByChatUUID[chat.Uuid]
		if !isOneToOne {
			continue // group-chat forward: indexed elsewhere
		}
		targets = append(targets, forwardedDMTarget{
			Chat:         chat,
			GroupingID:   dm.GroupingId,
			Participants: participants,
		})
	}
	return targets
}

func AddChatsAndPostsToOpensearch(dgraphPosts []*dgraphStruct.DgraphPost, dgraphDMs []*dgraphStruct.DgraphDm, HTMLText string, channelUUIDs []string, channelNames []string, userInfo *dgraphStruct.DgraphUser, participantsByChatUUID map[string][]*openSearchStruct.OpenSearchChatParticipants) {

	plainText := helpers.HTMLToPlainText(HTMLText)

	for i, dgraphPost := range dgraphPosts {

		openSearchPost := &openSearchStruct.OpenSearchPost{
			Uuid:               dgraphPost.Uuid,
			PostBody:           plainText,
			PostChannelUuid:    channelUUIDs[i],
			PostByUserUuid:     userInfo.Uuid,
			PostByProfile:      userInfo.ProfileKey,
			PostByUserFullName: userInfo.UserName,
			PostCreatedAt:      time.Now().Unix(),
			PostUpdatedAt:      time.Now().Unix(),
			PostDeletedAt:      nil,
			PostChannelName:    channelNames[i],
		}

		go postDomain.CreatePostWithAttachmentsInOpenSearch(openSearchPost, dgraphPost.MediaObj)
	}

	// Only 1:1 DM forwards are indexed here; group chats use a separate index path.
	for _, target := range forwardedDMTargets(dgraphDMs, participantsByChatUUID) {

		openSearchChat := &openSearchStruct.OpenSearchChat{
			Uuid:               target.Chat.Uuid,
			ChatBody:           plainText,
			ChatByProfile:      userInfo.ProfileKey,
			ChatByUserFullName: userInfo.UserName,
			ChatByUserUuid:     userInfo.Uuid,
			ChatGrpId:          target.GroupingID,
			ChatParticipants:   target.Participants,
			ChatCreatedAt:      time.Now().Unix(),
			ChatUpdatedAt:      time.Now().Unix(),
			ChatDeletedAt:      nil,
		}

		go chatDomain.CreateChatWithAttachmentsInOpenSearch(openSearchChat, target.Chat.MediaObj)

	}

}
