package business

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	LiveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// GetSignedProfileURL is now in userBusiness

type ChatPagination struct {
	Chats   []*dgraphStruct.DgraphChat `json:"chats,omitempty"`
	HasMore bool                       `json:"has_more"`
}

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

	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)

	isNewGroup := len(chatInfo.GrpUuid) == 0
	userUUIDs := uuidsOf(participantsDgraphRaw)

	if isNewGroup {
		chatInfo.GrpUuid, err = helpers.GenerateGroupID(userUUIDs)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateChatForGroup Failed to generate grp id err: %+v",
				err)
			return
		}
	}

	err = createChatRow(ctx, chatUUID, userInfo.UserPostgresInfo.Id, chatInfo.GrpUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatForGroup Failed to create new chat err: %+v",
			err)
		return
	}

	var newMembers []*dgraphStruct.DgraphUser
	if isNewGroup {
		newMembers = participantsDgraphRaw
	}
	replyParentChat, dmUID, err := writeGroup(ctx, chatUUID, chatInfo, userInfo, mentions, newMembers, currentTime)

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
	refsBusiness.Saved(ctx, refsBusiness.Chat, chatUUID.String(), chatInfo.TextHtml, true)

	// The message is stored from here on. A read mark that can't be written is
	// logged and the send goes on: returning told the sender it had failed,
	// so nothing else happened (no live message, no push, no search) and Try
	// again stored it twice.
	if len(dmUID) != 0 {
		startNewGroup(ctx, userInfo, chatInfo.GrpUuid, userUUIDs, currentTime)
	}
	markRead(ctx, "business/CreateChatForGroup", chatInfo.GrpUuid, userInfo, currentTime)

	createdChatInfo = &adapter.OutputCreateChatForChat{
		Uuid:        chatUUID.String(),
		ChatCreated: currentTime,
	}

	var opened []string
	if len(dmUID) != 0 {
		// A new group: nobody's open app holds its topic, the sender's
		// included. This message reaches them when they fetch it.
		opened = userUUIDs
	}
	fanOutCreatedChat(ctx, createdChat{
		kind: mqttStruct.CONVERSATION_KIND_GROUP, id: chatUUID.String(), grp: chatInfo.GrpUuid,
		html: chatInfo.TextHtml, files: chatInfo.MediaObjects, author: &userInfo.UserDgraphInfo,
		people: participantsDgraphRaw, opened: opened, mentions: mentions, replyTo: replyParentChat, at: currentTime,
	})

	return

}

// The steps today's writers take before their fan-out, seams for tests
// (golden_test.go), shared with the durable writes (durable.go).
var (
	createChatRow = domain.CreateChat
	writeDM       = writeDMNode
	writeGroup    = writeGroupNode
	startNewDM    = startDM
	startNewGroup = startGroup
	markRead      = markSentRead
)

// uuidsOf is the people's uuids, in their order. Pure.
func uuidsOf(people []*dgraphStruct.DgraphUser) []string {
	var out []string
	for _, u := range people {
		out = append(out, u.Uuid)
	}
	return out
}

// writeGroupNode writes a group message's graph node in its group's Dm node,
// with its files, its quote of a message in the same group and its mentions,
// and answers the message it quotes (nil for none) and the Dm node's uid when
// this message made the group (newMembers are its people then). Shared by
// today's CreateChatForGroup and the durable write (durable.go).
func writeGroupNode(ctx context.Context, chatUUID uuid.UUID, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, mentions []*dgraphStruct.DgraphUser, newMembers []*dgraphStruct.DgraphUser, currentTime time.Time) (*dgraphStruct.DgraphChat, string, error) {
	zeroUnixTime := time.Time{}
	// New nodes from the upload's own fields: never a node the request names
	// (models/dgraph NewAttachments, which stamps the adder and the time).
	chatInfo.MediaObjects = dgraphStruct.NewAttachments(chatInfo.MediaObjects, userInfo.UserDgraphInfo.Uid, currentTime)

	var participantsDgraph []*dgraphStruct.DgraphUser
	for _, u := range newMembers {
		participantsDgraph = append(participantsDgraph, &dgraphStruct.DgraphUser{
			Uid: u.Uid,
			DMs: []*dgraphStruct.DgraphDm{
				{
					Uid: "uid(dm)",
				},
			},
		})
	}

	// Inline reply (Discord-style): resolve + validate the reply target in the
	// SAME group (identical grouping id). Invalid / foreign / deleted targets
	// resolve to nil and are silently dropped — see resolveReplyParentChat.
	replyParentChat := resolveReplyParentChat(ctx, chatInfo.ReplyToUuid, chatInfo.GrpUuid)

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
				Mentions:    chatMentionsNode(chatUUID, mentions, currentTime),
			},
		},
	}

	_, dmUID, err := domain.CreateDgraphChat(ctx, &dgraphDm, chatUUID.String())
	return replyParentChat, dmUID, err
}

// chatMentionsNode is a message's Mention node: the people it names. Pure.
func chatMentionsNode(chatUUID uuid.UUID, mentions []*dgraphStruct.DgraphUser, currentTime time.Time) *dgraphStruct.DgraphMentions {
	var chatMentions []*dgraphStruct.DgraphUser
	for _, mention := range mentions {
		chatMentions = append(chatMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}
	return &dgraphStruct.DgraphMentions{
		Mentions:  chatMentions,
		ChatUuid:  chatUUID.String(),
		CreatedAt: &currentTime,
		Chat: &dgraphStruct.DgraphChat{
			Uid: "uid(ch)",
		},
	}
}

// startGroup is what a group's first message sets up beside it: the
// conversations index's rows, everyone's read marks (the others haven't seen
// it: it counts as unread, and no read receipt says otherwise) and their
// notification levels. A mark that can't be written is logged.
func startGroup(ctx context.Context, userInfo *userModels.UserInfo, grpID string, people []string, currentTime time.Time) {
	conversationDomain.Opened(ctx, conversationDomain.KindGroup, grpID, userInfo.UserDgraphInfo.Uuid, people, nil)
	if markErr := lastseenBusiness.BulkCreateLastSeenChatIfNotExists(ctx, people, grpID); markErr != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatForGroup Failed to update send to last seen chat err: %+v",
			markErr)
	}

	chatNotificationBusiness.BulkCreateChatNotification(ctx, people, grpID, postgressStruct.NOTIFICATION_TYPE_ALL, currentTime)
}

// markSentRead marks the conversation read for the sender up to their
// message; one that can't be written is logged, and the send goes on.
func markSentRead(ctx context.Context, where, grpID string, userInfo *userModels.UserInfo, currentTime time.Time) {
	if markErr := lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, grpID, userInfo.UserPostgresInfo.Id, currentTime); markErr != nil {
		helpers.LogErrorWithContext(ctx,
			"%s Failed to update last seen chat err: %+v",
			where, markErr)
	}
}

func CreateChat(ctx context.Context, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, sendToDgraph *dgraphStruct.DgraphUser, toUUID uuid.UUID, mentions []*dgraphStruct.DgraphUser) (createdChatInfo *adapter.OutputCreateChatForChat, err error) {

	// create channel uuid
	chatUUID := uuid.New()

	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)

	groupingId := helpers.GetGroupingId(userInfo.UserPostgresInfo.Id.String(), chatInfo.ToUuid)

	// create chat in postgres
	err = createChatRow(ctx, chatUUID, userInfo.UserPostgresInfo.Id, groupingId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChat Failed to create new chat err: %+v",
			err)
		return
	}

	replyParentChat, dmUID, err := writeDM(ctx, chatUUID, chatInfo, userInfo, sendToDgraph, groupingId, mentions, currentTime)

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
	refsBusiness.Saved(ctx, refsBusiness.Chat, chatUUID.String(), chatInfo.TextHtml, true)

	// The message is stored from here on: a read mark that can't be written
	// is logged and the send goes on (see CreateChatForGroup).
	if len(dmUID) != 0 {
		startNewDM(ctx, userInfo, sendToDgraph, toUUID, groupingId, currentTime)
	}

	markRead(ctx, "business/CreateChat", groupingId, userInfo, currentTime)

	createdChatInfo = &adapter.OutputCreateChatForChat{
		Uuid:        chatUUID.String(),
		ChatCreated: currentTime,
	}

	var opened []string
	if len(dmUID) != 0 {
		// A first DM: neither person's open app holds its topic.
		opened = []string{userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid}
	}
	fanOutCreatedChat(ctx, createdChat{
		kind: mqttStruct.CONVERSATION_KIND_DM, id: chatUUID.String(), grp: groupingId,
		html: chatInfo.TextHtml, files: chatInfo.MediaObjects, author: &userInfo.UserDgraphInfo, to: sendToDgraph,
		opened: opened, mentions: mentions, replyTo: replyParentChat, at: currentTime,
	})

	return
}

// writeDMNode writes a DM's graph node in the two people's Dm node (made when
// it's their first), with its files, its quote of a message in the same DM
// and its mentions, and answers the message it quotes (nil for none) and the
// Dm node's uid when this message made it. Shared by today's CreateChat and
// the durable write (durable.go).
func writeDMNode(ctx context.Context, chatUUID uuid.UUID, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, sendToDgraph *dgraphStruct.DgraphUser, groupingId string, mentions []*dgraphStruct.DgraphUser, currentTime time.Time) (*dgraphStruct.DgraphChat, string, error) {
	zeroUnixTime := time.Time{}
	// New nodes from the upload's own fields: never a node the request names
	// (models/dgraph NewAttachments, which stamps the adder and the time).
	chatInfo.MediaObjects = dgraphStruct.NewAttachments(chatInfo.MediaObjects, userInfo.UserDgraphInfo.Uid, currentTime)

	// Inline reply (Discord-style): resolve + validate the reply target in the
	// SAME DM. Invalid / foreign / deleted targets resolve to nil and are
	// silently dropped (the chat is still created) — see resolveReplyParentChat.
	replyParentChat := resolveReplyParentChat(ctx, chatInfo.ReplyToUuid, groupingId)

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
				Mentions:    chatMentionsNode(chatUUID, mentions, currentTime),
			},
		},
	}

	_, dmUID, err := domain.CreateDgraphChat(ctx, dgraphDm, chatUUID.String())
	return replyParentChat, dmUID, err
}

// startDM is what a DM's first message sets up beside it: the conversations
// index's rows, the other person's read mark (they haven't seen it) and the
// sender's notification level for it. A mark that can't be written is logged.
func startDM(ctx context.Context, userInfo *userModels.UserInfo, sendToDgraph *dgraphStruct.DgraphUser, toUUID uuid.UUID, groupingId string, currentTime time.Time) {
	conversationDomain.Opened(ctx, conversationDomain.KindDM, groupingId, userInfo.UserDgraphInfo.Uuid, []string{userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid}, nil)
	// The other person hasn't seen the new conversation: see CreateChatForGroup.
	if markErr := lastseenBusiness.BulkCreateLastSeenChatIfNotExists(ctx, []string{toUUID.String()}, groupingId); markErr != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChat Failed to update send to last seen chat err: %+v",
			markErr)
	}
	if userInfo.UserDgraphInfo.Uuid != sendToDgraph.Uuid {
		chatNotificationBusiness.CreateChatNotificationType(ctx, userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid, groupingId, postgressStruct.NOTIFICATION_TYPE_ALL, currentTime)

	}
}

// createdChat is a new DM or group message as its side effects tell of it.
type createdChat struct {
	// kind is mqttStruct's CONVERSATION_KIND_DM or _GROUP.
	kind     string
	id       string
	grp      string
	html     string
	files    []*dgraphStruct.DgraphAttachment
	author   *dgraphStruct.DgraphUser
	to       *dgraphStruct.DgraphUser   // a DM's other person
	people   []*dgraphStruct.DgraphUser // a group's people, as its writer gave them
	opened   []string                   // who is told the conversation is theirs, when this message made it
	mentions []*dgraphStruct.DgraphUser
	replyTo  *dgraphStruct.DgraphChat
	at       time.Time
}

// participants is who a message's search entry says may find it: a DM's two
// people, or a group's. Pure.
func (c createdChat) participants() []*openSearchStruct.OpenSearchChatParticipants {
	people := c.people
	if c.kind == mqttStruct.CONVERSATION_KIND_DM {
		people = []*dgraphStruct.DgraphUser{c.author, c.to}
	}
	var out []*openSearchStruct.OpenSearchChatParticipants
	for _, u := range people {
		out = append(out, &openSearchStruct.OpenSearchChatParticipants{
			Uuid:       u.Uuid,
			Name:       u.DisplayName(),
			ProfileKey: u.ProfileKey,
		})
	}
	return out
}

// livePayload is the live message open apps get. Pure.
func (c createdChat) livePayload() *mqttStruct.MqttChat {
	at := c.at
	return &mqttStruct.MqttChat{
		Type:             mqttStruct.TYPE_CREATE,
		ChatUuid:         c.id,
		ChatCreatedAt:    &at,
		ChatByProfileKey: c.author.ProfileKey,
		ChatByUserName:   c.author.DisplayName(),
		ChatByUserUuid:   c.author.Uuid,
		ChatByIsBot:      c.author.IsBot,
		ChatHtmlText:     c.html,
		ChatAttachments:  c.files,
		ChatGrpId:        c.grp,
		ChatReplyTo:      c.replyTo,
	}
}

// searchEntry is the message's search entry. Pure.
func (c createdChat) searchEntry() *openSearchStruct.OpenSearchChat {
	return &openSearchStruct.OpenSearchChat{
		Uuid:               c.id,
		ChatCreatedAt:      c.at.Unix(),
		ChatBody:           helpers.HTMLToPlainText(c.html),
		ChatByProfile:      c.author.ProfileKey,
		ChatByUserFullName: c.author.DisplayName(),
		ChatByUserUuid:     c.author.Uuid,
		ChatGrpId:          c.grp,
		ChatParticipants:   c.participants(),
		ChatDeletedAt:      nil,
	}
}

// pushTitle is the push's title: "Dm - " or "Group - " and the sender. Pure.
func (c createdChat) pushTitle() string {
	if c.kind == mqttStruct.CONVERSATION_KIND_DM {
		return fmt.Sprintf("Dm - %s", c.author.DisplayName())
	}
	return fmt.Sprintf("Group - %s", c.author.DisplayName())
}

// dmOther is a DM's other person, "" for a group. Pure.
func (c createdChat) dmOther() string {
	if c.kind != mqttStruct.CONVERSATION_KIND_DM || c.to == nil {
		return ""
	}
	return c.to.Uuid
}

// participantUUIDs is the people the embedding and the event name: a DM's
// two, or a group's as its writer gave them. Pure.
func (c createdChat) participantUUIDs() []string {
	var out []string
	for _, p := range c.participants() {
		out = append(out, p.Uuid)
	}
	return out
}

// createdEvent is the workspace event a DM or group message raises. Pure.
func (c createdChat) createdEvent() map[string]interface{} {
	event := map[string]interface{}{
		"message_id":  c.id,
		"sender_id":   c.author.Uuid,
		"group_id":    c.grp,
		"text":        helpers.HTMLToPlainText(c.html),
		"mention_ids": chatMentionNodeIDs(c.mentions),
	}
	if c.kind == mqttStruct.CONVERSATION_KIND_DM {
		event["receiver_id"] = c.dmOther()
	} else {
		event["participant_ids"] = c.participantUUIDs()
	}
	return event
}

// The seams today's fan-out and the durable sinks go out through, for tests.
var (
	publishChat       = mqttBusiness.PublishChat
	publishAccess     = mqttBusiness.PublishConversationAccess
	notifyChat        = notifyNewChat
	createChatEntry   = domain.CreateChatWithAttachmentsInOpenSearch
	dispatchChatEvent = webhookBusiness.DispatchEvent
)

// fanOutCreatedChat is today's side effects of a new DM or group message,
// after its node is written: what the durable pipeline's sinks do from the
// outbox instead (chatSinks.go), through the same seams, so golden_test.go
// can hold the two to one answer. An import's message is only indexed.
func fanOutCreatedChat(ctx context.Context, c createdChat) {
	bulk := helpers.IsBulkImport(ctx)
	if !bulk {
		live := c.livePayload()
		helpers.GoSafeNamed("chat.publish", func() { publishChat(live, c.grp) })
		if len(c.opened) > 0 {
			opened := c.opened
			helpers.GoSafeNamed("chat.access", func() {
				publishAccess(c.kind, c.grp, mqttStruct.CONVERSATION_ADDED, opened...)
			})
		}
	}

	entry := c.searchEntry()
	if !bulk {
		replyTo := replyParentChatAuthorUUID(c.replyTo)
		helpers.GoSafeNamed("chat.notify", func() {
			notifyChat(context.Background(), c.pushTitle(), entry.ChatBody, c.author.Uuid, c.grp, c.mentions, c.id, c.author.DisplayName(), c.author.ProfileKey, replyTo, nil)
		})
	}
	helpers.GoSafeNamed("chat.search", func() { createChatEntry(entry, c.files) })

	if !bulk {
		// embed for AI Second Brain (async). Skipped during imports; a
		// follow-up batch embed run handles imported content. For a reply,
		// embed the parent snippet + reply text (contextual embedding);
		// same-conversation guarantees no permission leak.
		/* AI call omitted in v1 */

		event := c.createdEvent()
		detached := context.WithoutCancel(ctx)
		helpers.GoSafeNamed("chat.events", func() { dispatchChatEvent(detached, "chat.created", event) })
	}
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

// notifyNewChat is a DM or group message's notices: the Activity item for
// each eligible person it mentions (or whose message it quotes), the push and
// desktop notice to every eligible person, and the email fan-out. tell, when
// set, lets through only those not told yet (the outbox's sink: a retry tells
// nobody twice); nil tells everyone, as today's writers do.
func notifyNewChat(ctx context.Context, title string, body string, userId string, grpId string, mentions []*dgraphStruct.DgraphUser, chatUUID string, username string, profileKey *string, replyToAuthorUUID string, tell outboxBusiness.Told) {

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
			"business/notifyNewChat Failed to get eligible users err: %+v",
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

	var mentioned []string
	for _, recipientID := range eligibleUserIDs {
		// Only send the activity broadcast if the user was explicitly mentioned
		if mentionsMap[recipientID] {
			mentioned = append(mentioned, recipientID)
		}
	}
	for _, recipientID := range tell.Only("activity", mentioned) {

		// Create a simplified activity item for MQTT
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
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

	// 3. The push, and the desktop app's notice, to those of them the push
	// gate lets through (business/Notification Deliver).
	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = chatUUID
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = username
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, profileKey)

	if pushTo := tell.Only("push", eligibleUserIDs); len(pushTo) > 0 {
		notificationBusiness.DeliverFrom(ctx, userId, pushTo, pushData, "chat:"+grpId+":"+chatUUID)
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
		tell.Only("email", eligibleUserIDs),
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
		Uid:  "uid(cha)",
		Uuid: chatInfo.Uuid,
		Body: chatInfo.TextHtml,
		// An edit changes the text and who it names, never its files (as a
		// post's and a comment's edits don't): the request's files were
		// written as they came, so a uid in one linked and overwrote any node.
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
	refsBusiness.Saved(ctx, refsBusiness.Chat, chatUUID.String(), chatInfo.TextHtml, false)

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
	helpers.GoSafeNamed("domain.UpdateChatInOpenSearch", func() { domain.UpdateChatInOpenSearch(openSearchChat) })

	// Suppress live fan-out during a bulk Slack import. The Postgres +
	// Dgraph + OpenSearch writes above land so search and timelines are
	// consistent; the MQTT/AI/webhook events are skipped to avoid
	// notifying every participant of historical edits.
	if helpers.IsBulkImport(ctx) {
		return
	}

	helpers.GoSafeNamed("mqttBusiness.PublishChat", func() { mqttBusiness.PublishChat(&mqttChat, grpId) })

	// Re-embed for AI Second Brain with updated content (async)
	/* AI call omitted in v1 */

	// Reconcile any workspace-memory items captured from this DM/group
	// message: refresh the stored snapshot to the edited text, or drop it
	// if the edit emptied the message. (Mirrors UpdatePost.)
	/* AI call omitted in v1 */

	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.updated", map[string]interface{}{
			"message_id": chatUUID.String(),
		})
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

	helpers.GoSafeNamed("domain.DeleteChatWithAttachmentsInOpenSearch", func() { domain.DeleteChatWithAttachmentsInOpenSearch(openSearchChat, rawDgraphChat.MediaObj) })
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

	helpers.GoSafeNamed("mqttBusiness.PublishChat", func() { mqttBusiness.PublishChat(&mqttChat, grpId) })

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
	/* AI call omitted in v1 */

	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "chat.deleted", map[string]interface{}{
			"message_id": chatUUID.String(),
		})
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
	dgraphUser.DMs = conversationDomain.WithoutHidden(ctx, userUUID.String(), dgraphUser.DMs)

	return
}

// GetDgraphOldGroupChatFromDgraph is the page of a group's messages before
// edge (helpers.PageEdge).
func GetDgraphOldGroupChatFromDgraph(ctx context.Context, edge helpers.PageEdge, grpID string) (chats ChatPagination, err error) {
	dgraphChats, err := domain.GetDgraphOldChatFromDgraph(ctx, grpID, edge.Before())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphOldGroupChatFromDgraph Failed to get old chats from dgraph err: %+v",
			err)

		return
	}

	chats.Chats, chats.HasMore = helpers.PageFrom(dgraphChats, chatUUIDOf, edge, domain.CHAT_COUNT)

	return
}

// GetDgraphOldChatFromDgraph is the page of a DM's messages before edge.
func GetDgraphOldChatFromDgraph(ctx context.Context, edge helpers.PageEdge, firstUserUUID string, secondUserUUID string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(firstUserUUID, secondUserUUID)
	dgraphChats, err := domain.GetDgraphOldChatFromDgraph(ctx, groupingId, edge.Before())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphOldChatFromDgraph Failed to get old chats from dgraph err: %+v",
			err)

		return
	}

	chats.Chats, chats.HasMore = helpers.PageFrom(dgraphChats, chatUUIDOf, edge, domain.CHAT_COUNT)

	return
}

// GetDgraphNewGroupChatFromDgraph is the page of a group's messages after
// edge.
func GetDgraphNewGroupChatFromDgraph(ctx context.Context, edge helpers.PageEdge, grpId string) (chats ChatPagination, err error) {
	dgraphChats, err := domain.GetDgraphNewChatFromDgraph(ctx, grpId, edge.After())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewGroupChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	chats.Chats, chats.HasMore = helpers.PageFrom(dgraphChats, chatUUIDOf, edge, domain.CHAT_COUNT)

	return
}

// GetDgraphNewChatFromDgraph is the page of a DM's messages after edge.
func GetDgraphNewChatFromDgraph(ctx context.Context, edge helpers.PageEdge, fromUserId string, toUserId string) (chats ChatPagination, err error) {
	groupingId := helpers.GetGroupingId(fromUserId, toUserId)
	dgraphChats, err := domain.GetDgraphNewChatFromDgraph(ctx, groupingId, edge.After())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewChatFromDgraph Failed to get new chats from dgraph err: %+v",
			err)

		return
	}

	chats.Chats, chats.HasMore = helpers.PageFrom(dgraphChats, chatUUIDOf, edge, domain.CHAT_COUNT)

	return
}

func chatUUIDOf(c *dgraphStruct.DgraphChat) string { return c.Uuid }

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

// reactionPushTo is the devices a reaction in a direct or group conversation
// is pushed to: its author's, while their setting for the conversation is
// "all" or "mention" (a reaction to their message is addressed to them; with
// "block", or no setting, nothing), and not while their notifications are
// paused, never the reactor's own.
func reactionPushTo(ctx context.Context, grpID, authorUUID, reactorUUID string) ([]string, error) {
	if grpID == "" || authorUUID == "" || authorUUID == reactorUUID {
		return nil, nil
	}
	told, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grpID, reactorUUID, []string{authorUUID}, true)
	if err != nil || len(told) == 0 {
		return nil, err
	}
	return userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, told)
}

func sendNewChatReactionNotification(title string, body string, grpId string, chatDgraph *dgraphStruct.DgraphChat, userDgraph *dgraphStruct.DgraphUser, reactionID string) {

	if chatDgraph.From.Uuid == userDgraph.Uuid {
		return
	}

	ctx := context.Background()

	activityItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_REACTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
		Time:         time.Now().UTC().Format(time.RFC3339Nano),
	}

	activityBusiness.PublishActivityToUser(chatDgraph.From.Uuid, activityItem)

	// The push goes to the message's author, as their setting for the
	// conversation allows (reactionPushTo). It went to whoever reacted.
	tokens, err := reactionPushTo(ctx, grpId, chatDgraph.From.Uuid, userDgraph.Uuid)

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

	// New nodes from the upload's own fields: never a node the request names.
	commentInfo.MediaObj = dgraphStruct.NewAttachments(commentInfo.MediaObj, userInfo.UserDgraphInfo.Uid, currentTime)

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
		helpers.GoSafeNamed("mqttBusiness.PublishChatComment", func() { mqttBusiness.PublishChatComment(&mqttChatComment, grpId) })

		go notifyChatThreadReply(currentTime, commentUUID.String(), commentInfo.HTMLText, &userInfo.UserDgraphInfo, grpId, mentionsDgraphUsersList, chatDgraphInfo)

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

// Who a reply in a DM or group thread is told to, the ways it is told, and
// the thread's index (business/ThreadIndex, chat P2): seams for its test.
var (
	threadLevels      = chatNotificationBusiness.GetLevelsInConversation
	threadRepliers    = commentBusiness.ChatThreadRepliers
	threadFollows     = threadIndex.Following
	recordThreadReply = threadIndex.Replied
	deliverTo         = notificationBusiness.Deliver
	publishActivityTo = activityBusiness.PublishActivityToUser
	emailChatComment  = notificationBusiness.DispatchChatComment
)

// chatThreadAudience is who hears of a reply by writer in the thread of the
// message chat: its author, those who replied before, those following it and
// those mentioned, by their setting for the conversation, never one who
// turned the thread off and isn't mentioned (notificationBusiness.ThreadAudience,
// the rule a channel thread's reply uses). In a conversation of two the other
// person is in every thread of it: a DM is addressed to them.
func chatThreadAudience(ctx context.Context, grpId, writer string, chat *dgraphStruct.DgraphChat, mentioned []string) (notificationBusiness.Audience, error) {
	author := ""
	if chat.From != nil {
		author = chat.From.Uuid
	}
	repliers, err := threadRepliers(ctx, chat.Uuid)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	on, off, err := threadFollows(ctx, chat.Uuid)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	if chat.DM != nil && len(chat.DM.Participants) == 2 {
		for _, p := range chat.DM.Participants {
			if p != nil && !strings.EqualFold(p.Uuid, writer) {
				repliers = append([]string{p.Uuid}, repliers...)
			}
		}
	}
	// Each person is asked about once, and only by a well-formed id: one
	// malformed id would fail the whole read, and tell nobody.
	var ask []string
	for _, id := range slices.Concat([]string{author}, repliers, on, mentioned) {
		if _, err := uuid.Parse(id); err == nil && !slices.Contains(ask, id) {
			ask = append(ask, id)
		}
	}
	levels, err := threadLevels(ctx, grpId, ask)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	return notificationBusiness.ThreadAudience(levels, writer, author, repliers, mentioned,
		notificationBusiness.Follows{On: on, Off: off}), nil
}

// notifyChatThreadReply records a reply in a DM or group thread in the
// thread's index (business/ThreadIndex) and tells of it: the push and the
// desktop notice, the live Activity items (the author's "replied to your
// message", a mention item to each one mentioned) and the email, all to one
// audience (chatThreadAudience). They were two lists: push to everyone in the
// conversation on "all", Activity and email to the author and those
// mentioned. When the audience can't be worked out, nobody is told. It takes
// its time; run it on its own goroutine.
func notifyChatThreadReply(currentTime time.Time, commentUUID string, commentBody string, userDgraph *dgraphStruct.DgraphUser, grpId string, mentions []*dgraphStruct.DgraphUser, chatDgraph *dgraphStruct.DgraphChat) {
	if userDgraph == nil || chatDgraph == nil {
		return
	}
	ctx := context.Background()
	mentioned := dgraphStruct.UUIDsOf(mentions)
	// The thread's index first, so the Threads view has the reply and the
	// audience counts who follows the thread now.
	recordThreadReply(ctx, threadIndex.Reply{
		Thread: threadIndex.OfChat(grpId, chatDgraph), By: userDgraph.Uuid, Mentioned: mentioned, At: currentTime,
	})
	aud, err := chatThreadAudience(ctx, grpId, userDgraph.Uuid, chatDgraph, mentioned)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/notifyChatThreadReply Failed to work out who hears of the reply, telling nobody err: %+v",
			err)
		return
	}
	if len(aud.Told) == 0 {
		return
	}
	plain := helpers.HTMLToPlainText(commentBody)
	icon := userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	// The push, and the desktop app's notice, to those of them the push gate
	// lets through (business/Notification Deliver). type_id is the
	// conversation's grouping id, as for every chat notification.
	deliverTo(ctx, aud.Told, map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   grpId,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:     "Comment - " + userDgraph.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_BODY:      plain,
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: chatDgraph.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME:  userDgraph.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_ICON:      icon,
	}, "chat_comment:"+chatDgraph.Uuid+":"+commentUUID)

	// The live Activity items.
	comment := func() *dgraphStruct.DgraphComment {
		at := currentTime
		return &dgraphStruct.DgraphComment{
			Uuid:      commentUUID,
			Text:      commentBody,
			CommentBy: &dgraphStruct.DgraphUser{Uuid: userDgraph.Uuid, UserName: userDgraph.DisplayName()},
			Chat:      &dgraphStruct.DgraphChat{Uuid: chatDgraph.Uuid},
			CreatedAt: &at,
		}
	}
	for _, id := range aud.Mentioned {
		publishActivityTo(id, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}
	if aud.Author != "" {
		publishActivityTo(aud.Author, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}

	// Email, by each one's own email settings (business/Notification).
	emailChatComment(userDgraph.Uuid, userDgraph.DisplayName(), icon, grpId, chatDgraph.Uuid, plain, commentUUID, aud.Told)
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

	helpers.GoSafeNamed("mqttBusiness.PublishChatComment", func() { mqttBusiness.PublishChatComment(&mqttChatComment, rawDgraphChat.DM.GroupingId) })

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

	helpers.GoSafeNamed("mqttBusiness.PublishChatComment", func() { mqttBusiness.PublishChatComment(&mqttChatComment, rawDgraphComment.ChatGroupingId) })

	return
}

func sendNewChatCommentReactionNotification(title string, body string, commentDgraph *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser, reactionID string) {

	if commentDgraph.CommentBy.Uuid == userDgraph.Uuid {
		return
	}

	ctx := context.Background()

	activityItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_REACTION, // In chats, it's either a mention or a new message (which we'll treat as a notification)
		Time:         time.Now().UTC().Format(time.RFC3339Nano),
	}

	activityBusiness.PublishActivityToUser(commentDgraph.CommentBy.Uuid, activityItem)

	// The push goes to the reply's author, as their setting for the
	// conversation allows (reactionPushTo). It went to whoever reacted.
	grpID := commentDgraph.ChatGroupingId
	if grpID == "" && commentDgraph.Chat != nil && commentDgraph.Chat.DM != nil {
		grpID = commentDgraph.Chat.DM.GroupingId
	}
	tokens, err := reactionPushTo(ctx, grpID, commentDgraph.CommentBy.Uuid, userDgraph.Uuid)

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

func PublishChatTyping(userDgraph *dgraphStruct.DgraphUser, grpId string) {
	mqttChatTyping := mqttStruct.MqttChatTyping{
		UserUUID:  userDgraph.Uuid,
		UserName:  userDgraph.DisplayName(),
		ChatGrpId: grpId,
	}

	if userDgraph.ProfileKey != nil {
		mqttChatTyping.UserProfile = *userDgraph.ProfileKey
	}
	helpers.GoSafeNamed("mqttBusiness.PublishChatTyping", func() { mqttBusiness.PublishChatTyping(&mqttChatTyping, grpId) })
}

func MakeVideoCall(ctx context.Context, grpId string, userDraphInfo *dgraphStruct.DgraphUser, isAdmin bool, language string, isGroup bool) (token string, alreadyExisted bool, err error) {

	token, alreadyExisted, err = LiveKitBusiness.CreateRoomAndGetToken(ctx, grpId, userDraphInfo, isAdmin, language)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoCall Failed to make video call post err: %+v",
			err)

		return
	}

	// Its ring and its call dot wait for the first person to really join
	// (callStartedPush.go): a token minted for a green room nobody left
	// rings nobody.
	return
}

func PublishStopChatCall(grpId string) {

	mqttChatCall := mqttStruct.MqttChatCall{
		CallActive: mqttStruct.MESSAGE_CALL_INACTIVE,
		GrpId:      grpId,
	}

	helpers.GoSafeNamed("mqttBusiness.PublishChatCall", func() { mqttBusiness.PublishChatCall(&mqttChatCall, grpId) })

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

	// 2. The push, and the desktop app's notice, to those of them the push
	// gate lets through (business/Notification Deliver). Each call rings
	// anew, so its notice is named by when it started.
	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = username
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, profileKey)

	notificationBusiness.Deliver(ctx, eligibleUserIDs, pushData, fmt.Sprintf("chat_call:%s:%d", grpId, time.Now().UnixMilli()))

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
	_, participates, err := GroupingParticipation(ctx, groupingId, userDgraphUID)
	return participates, err
}

// GroupingParticipation is ParticipatesInGrouping that also says whether the
// grouping has started: a group is written at its first message, and its id is
// made from its people before then (helpers.GenerateGroupID). It is for a
// caller that must hold a group not started yet to another rule (an upload to
// one names its people); what it tells the person must not say which, for the
// reason above.
func GroupingParticipation(ctx context.Context, groupingId string, userDgraphUID string) (started, participates bool, err error) {
	groupingId = strings.TrimSpace(groupingId)
	userDgraphUID = strings.TrimSpace(userDgraphUID)
	if groupingId == "" || userDgraphUID == "" {
		return false, false, nil
	}

	dm, err := domain.GetDmParticipation(ctx, groupingId, userDgraphUID)
	if err != nil || dm == nil {
		return false, false, err
	}
	return true, dm.ParticipantIsMember > 0, nil
}
