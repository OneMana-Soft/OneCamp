package business

import (
	"context"
	"strings"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// tellChatMentionsAdded tells, in the background, whom an edit of a DM's or a
// group's message (or of a reply in its thread, commentUUID) newly names (s,
// read before the edit's write) through the one door, as a send's mention is
// told (business/Notify TellAddedMentions: of those in it now, by their
// setting, a row per delivery about the message the edit is of, retracted
// when it is deleted): in the inbox, by push and by email
// (message.mentioned).
func tellChatMentionsAdded(ctx context.Context, actor *dgraphStruct.DgraphUser, grpID, chatUUID, commentUUID, html string, s notificationBusiness.MentionSave) {
	if actor == nil || grpID == "" {
		return
	}
	data := map[string]string{"message_id": chatUUID}
	push := &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT, ID: grpID, Thread: chatUUID}
	if commentUUID != "" {
		data["comment_uuid"] = commentUUID
		push.Type = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT
	}
	// The path opens the conversation as whoever is told sees it: a DM's
	// other person is the actor; a group's is the group.
	viewer := ""
	for _, id := range strings.Fields(grpID) {
		if id != actor.Uuid {
			viewer = id
		}
	}
	n := notificationBusiness.Notice{
		Kind:          notificationBusiness.MessageMentioned,
		ResourceKind:  chatNotificationBusiness.NoticeKind,
		ResourceUUID:  grpID,
		ResourceTitle: "a conversation",
		Actor:         actor,
		Data:          data,
		Title:         actor.DisplayName() + " mentioned you in a conversation",
		Body:          helpers.HTMLToPlainText(html),
		Path:          helpers.ChatPath(grpID, viewer, chatUUID),
		Email:         notificationBusiness.EventGroupChatMention,
		Push:          push,
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("business/Chat tellChatMentionsAdded", func() {
		notifyBusiness.TellAddedMentions(ctx, notifyBusiness.Place{Kind: notifyBusiness.PlaceDM, ID: grpID}, n, s)
	})
}
