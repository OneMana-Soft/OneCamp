package business

import (
	"context"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// tellPostMentionsAdded tells, in the background, whom an edit of a channel's
// message (or of a reply in its thread, commentUUID) newly names (s, read
// before the edit's write) through the one door, as a send's mention is told
// (business/Notify TellAddedMentions: of the channel's people alone, by their
// setting, a row per delivery about the message the edit is of, retracted
// when it is deleted): in the inbox, by push and, when the words name them
// themselves, by email (message.mentioned).
func tellPostMentionsAdded(ctx context.Context, actor *dgraphStruct.DgraphUser, channelID, postUUID, commentUUID, html string, mentions []*dgraphStruct.DgraphUser, s notificationBusiness.MentionSave) {
	if actor == nil || channelID == "" || len(notificationBusiness.MentionsAdded(s.Before, s.After, actor.Uuid)) == 0 {
		return
	}
	named := namedIn(html, mentions)
	s.Named = func(id string) bool { return named[id] }
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("business/Post tellPostMentionsAdded", func() {
		where := "a channel"
		if names, err := channelDomain.GetChannelNamesByUUIDs(ctx, []string{channelID}); err == nil && names[channelID] != "" {
			where = "#" + names[channelID]
		}
		notifyBusiness.TellAddedMentions(ctx, notifyBusiness.Place{Kind: notifyBusiness.PlaceChannel, ID: channelID}, postMentionNotice(actor, channelID, where, postUUID, commentUUID, html), s)
	})
}

// postMentionNotice is how a mention an edit adds to a channel's message (or
// to a reply in its thread, commentUUID) reads, where being the channel's
// name as told ("#launch", or "a channel"). Pure.
func postMentionNotice(actor *dgraphStruct.DgraphUser, channelID, where, postUUID, commentUUID, html string) notificationBusiness.Notice {
	data := map[string]string{"post_id": postUUID}
	push := &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL, ID: channelID, Thread: postUUID}
	if commentUUID != "" {
		data["comment_uuid"] = commentUUID
		push.Type = firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT
	}
	return notificationBusiness.Notice{
		Kind:          notificationBusiness.MessageMentioned,
		ResourceKind:  channelBusiness.NoticeKind,
		ResourceUUID:  channelID,
		ResourceTitle: where,
		Actor:         actor,
		Data:          data,
		Title:         actor.DisplayName() + " mentioned you in " + where,
		Body:          helpers.HTMLToPlainText(html),
		Path:          "/app/channel/" + channelID + "/" + postUUID,
		Email:         notificationBusiness.EventChannelMention,
		Push:          push,
		// Its email is asked again at the send: the post in its channel, and
		// the reply when there is one, as someone it names.
		About: &notificationBusiness.About{Kind: notificationBusiness.AboutChannel, Item: postUUID, Place: channelID, Reply: commentUUID, Heard: notifylevel.Named},
	}
}
