package business

// Reactions on a DM's and a group's messages and on their replies (chat B25):
// written once per person and emoji through business/Reaction, and told only
// for what really changed. The message's author hears of someone's reaction
// once, on that person's first reaction to it: several emoji from one person
// are one notice, as a single reaction was before.

import (
	"context"
	"fmt"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// reactedSnippet is what a reaction's push quotes of what was reacted to.
// Pure.
func reactedSnippet(body string) string {
	if len(body) > 30 {
		return helpers.TruncateRunesWithSuffix(body, 30, "...")
	}
	return body
}

// CreateOrUpdateChatReaction adds a reaction to a DM's or group's message as
// userDgraph, once per emoji, and tells the conversation and the message's
// author of what really changed. The answer is the reaction's node, the one
// added or the one the person already had.
func CreateOrUpdateChatReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForChat, chatDgraph *dgraphStruct.DgraphChat, userDgraph *dgraphStruct.DgraphUser, grpId string) (reactionUID string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnChat, UUID: reactionInfo.Uuid}
	if chatDgraph.From != nil {
		parent.ContentBy = chatDgraph.From.Uid
	}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraph.Uid, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiReactionUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdateChatReaction err: %+v", err)
		}
		return "", err
	}
	if helpers.IsBulkImport(ctx) {
		return added.UID, nil
	}
	tellChatReactionsGone(reactionInfo.Uuid, userDgraph.Uuid, grpId, removed)
	if !added.Created && !added.Retagged {
		return added.UID, nil
	}
	live := &mqttStruct.MqttChatReaction{
		Type: added.Told(), EmojiReactionId: reactionInfo.EmojiReactionUuid, ChatUuid: reactionInfo.Uuid,
		AddedByUuid: userDgraph.Uuid, AddedByUserName: userDgraph.DisplayName(), ChatGrpId: grpId, ReactionUuid: added.UID,
	}
	helpers.GoSafeNamed("mqttBusiness.PublishChatReaction", func() { mqttBusiness.PublishChatReaction(live, grpId) })

	if added.First {
		title := fmt.Sprintf("%s reacted", userDgraph.DisplayName())
		body := fmt.Sprintf("Reacted to: \"%s\"", reactedSnippet(chatDgraph.Body))
		told := reactionTold{
			kind: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, grpID: grpId, thread: chatDgraph.Uuid, on: chatDgraph.Uuid,
			by: userDgraph, emoji: reactionInfo.EmojiReactionUuid, title: title, body: body,
		}
		if chatDgraph.From != nil {
			told.author = chatDgraph.From.Uuid
		}
		helpers.GoSafeNamed("tellOfReaction", func() { tellOfReaction(told) })
	}
	return added.UID, nil
}

// tellChatReactionsGone tells the conversation of each reaction taken back,
// one live event apiece as the apps read them (usually one: a person holds
// one copy of an emoji).
func tellChatReactionsGone(chatUUID, byUUID, grpID string, removed []reactionBusiness.Removed) {
	for _, r := range removed {
		gone := &mqttStruct.MqttChatReaction{
			Type: mqttStruct.TYPE_DELETE, ChatUuid: chatUUID, AddedByUuid: byUUID,
			ChatGrpId: grpID, ReactionUuid: r.UID, EmojiReactionId: r.Emoji,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishChatReaction", func() { mqttBusiness.PublishChatReaction(gone, grpID) })
	}
}

// DeleteChatReaction takes back userDgraph's reaction reactionUID on a DM's or
// group's message, with any copy of the same emoji of theirs, and tells the
// conversation.
func DeleteChatReaction(ctx context.Context, chatUUID, reactionUID string, userDgraph *dgraphStruct.DgraphUser, grpId string) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnChat, UUID: chatUUID}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraph.Uid, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeleteChatReaction err: %+v", err)
		}
		return err
	}
	tellChatReactionsGone(chatUUID, userDgraph.Uuid, grpId, removed)
	return nil
}

// chatCommentTopic is the conversation a DM's or group's reply belongs to.
func chatCommentTopic(comment *dgraphStruct.DgraphComment) string {
	return helpers.GetGroupingId(comment.Chat.From.Uuid, comment.Chat.DM.GroupingId)
}

// CreateOrUpdateChatCommentReaction adds a reaction to a reply in a DM's or
// group's thread as userDgraph, once per emoji, and tells the conversation
// and the reply's author of what really changed.
func CreateOrUpdateChatCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInChat, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: reactionInfo.Uuid}
	if dgraphCommentRaw.CommentBy != nil {
		parent.ContentBy = dgraphCommentRaw.CommentBy.Uid
	}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraph.Uid, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiReactionUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdateChatCommentReaction err: %+v", err)
		}
		return "", err
	}

	// A bulk Slack import writes the reaction and tells nobody about
	// historical reactions.
	if helpers.IsBulkImport(ctx) {
		return added.UID, nil
	}
	tellChatCommentReactionsGone(dgraphCommentRaw, userDgraph.Uuid, removed)
	if !added.Created && !added.Retagged {
		return added.UID, nil
	}
	live := &mqttStruct.MqttChatCommentReaction{
		Type: added.Told(), EmojiReactionId: reactionInfo.EmojiReactionUuid, CommentUuid: reactionInfo.Uuid,
		AddedByUserName: userDgraph.DisplayName(), AddedByUuid: userDgraph.Uuid, ReactionUuid: added.UID,
		ChatUuid: dgraphCommentRaw.Chat.Uuid,
	}
	topic := chatCommentTopic(dgraphCommentRaw)
	helpers.GoSafeNamed("mqttBusiness.PublishChatCommentReaction", func() { mqttBusiness.PublishChatCommentReaction(live, topic) })

	if added.First {
		title := fmt.Sprintf("%s reacted", userDgraph.DisplayName())
		body := fmt.Sprintf("Reacted to: \"%s\"", reactedSnippet(dgraphCommentRaw.Text))
		// A reply's conversation, read from its root where the reply doesn't
		// name it (chatCommentTopic has read the root above).
		grpID := dgraphCommentRaw.ChatGroupingId
		if grpID == "" {
			grpID = dgraphCommentRaw.Chat.DM.GroupingId
		}
		told := reactionTold{
			kind: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT_REACTION, grpID: grpID, thread: dgraphCommentRaw.Chat.Uuid, on: dgraphCommentRaw.Uuid,
			by: userDgraph, emoji: reactionInfo.EmojiReactionUuid, title: title, body: body,
		}
		if dgraphCommentRaw.CommentBy != nil {
			told.author = dgraphCommentRaw.CommentBy.Uuid
		}
		helpers.GoSafeNamed("tellOfReaction", func() { tellOfReaction(told) })
	}
	return added.UID, nil
}

// tellChatCommentReactionsGone tells the conversation of each reaction taken
// back from a reply, one live event apiece as the apps read them.
func tellChatCommentReactionsGone(comment *dgraphStruct.DgraphComment, byUUID string, removed []reactionBusiness.Removed) {
	topic := chatCommentTopic(comment)
	for _, r := range removed {
		gone := &mqttStruct.MqttChatCommentReaction{
			Type: mqttStruct.TYPE_DELETE, CommentUuid: comment.Uuid, ReactionUuid: r.UID,
			EmojiReactionId: r.Emoji, ChatUuid: comment.Chat.Uuid, AddedByUuid: byUUID,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishChatCommentReaction", func() { mqttBusiness.PublishChatCommentReaction(gone, topic) })
	}
}

// DeleteReactionOnCommentChat takes back the person's reaction reactionUID on
// a DM's or group's reply, with any copy of the same emoji of theirs, and
// tells the conversation.
func DeleteReactionOnCommentChat(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionUID string, userDgraph *dgraphStruct.DgraphUser) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: commentDgraph.Uuid}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraph.Uid, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeleteReactionOnCommentChat err: %+v", err)
		}
		return err
	}
	tellChatCommentReactionsGone(commentDgraph, userDgraph.Uuid, removed)
	return nil
}

// reactionTold is someone's first reaction to a message or a reply in a DM or
// a group, as its author is told of it.
type reactionTold struct {
	kind   string // the push's type: a reaction to a message or to a reply
	grpID  string // the conversation's grouping id
	thread string // the thread the push opens: the message, or the reply's root
	on     string // what was reacted to
	author string
	by     *dgraphStruct.DgraphUser
	emoji  string
	title  string
	body   string
}

// tellOfReaction tells the author of a message or a reply of a reaction to it,
// never their own: on their activity, and by push and desktop notice through
// the one way a push goes out (notificationBusiness.Deliver: their devices,
// the desktop app, and pause, quiet hours and focus time). The push goes as
// their setting for the conversation allows: "all" or "mention" (a reaction
// to their message is addressed to them), never "block" or no setting. An
// author who has left the group hears nothing of it, on their activity
// either; when who is in it can't be read, nobody is told.
func tellOfReaction(r reactionTold) {
	if r.author == "" || r.author == r.by.Uuid {
		return
	}
	ctx := context.Background()
	if r.grpID != "" {
		in, err := chatNotificationBusiness.InConversationNow(ctx, r.grpID, []string{r.author})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Chat tellOfReaction could not read who is in %s now: %+v", r.grpID, err)
			return
		}
		if len(in) == 0 {
			return
		}
	}
	activityBusiness.PublishActivityToUser(r.author, &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_REACTION,
		Time:         time.Now().UTC().Format(time.RFC3339Nano),
	})
	if r.grpID == "" {
		return
	}
	hearers, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, r.grpID, r.by.Uuid, []string{r.author}, true)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat tellOfReaction could not read who hears of %s: %+v", r.on, err)
		return
	}
	notificationBusiness.Deliver(ctx, hearers, map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:        r.kind,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:     r.grpID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:       r.title,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:        r.body,
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID:   r.thread,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME:    r.by.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_ICON:        userBusiness.GetSignedProfileURL(ctx, r.by.ProfileKey),
		firebaseInit.FIREBASE_PUSH_DATA_REACTION_ID: r.emoji,
	}, fmt.Sprintf("%s:%s:%s:%s", r.kind, r.grpID, r.on, r.by.Uuid)) // one notice per person who reacted
}
