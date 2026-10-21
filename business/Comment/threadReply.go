package business

// Who hears of a reply in a channel thread, and how.
//
// A reply used to be told of three ways, each from a list of its own. Push went
// to those who had replied before when their setting for the channel was
// "all", and to the post's author and those mentioned only when it was
// "mention": on the default, "all", the very people a reply is addressed to
// were never pushed, and on "Mentions and threads you're in" a thread you had
// replied in never was. Email went to all of them unchecked, so someone
// mentioned from outside a private channel, or removed from it since, was sent
// the reply's text. And the post's author was always sent the live Activity
// item.
//
// Now one audience is worked out, from the channel's members as they are when
// the reply is written, and push, email and the live Activity items all go to
// it (notificationBusiness.ThreadAudience, which DM and group threads use too).

import (
	"context"
	"slices"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/Comment"
	channelNotificationDomain "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/google/uuid"
)

// ThreadReply is a reply in a channel thread, as its notices tell of it.
type ThreadReply struct {
	// By wrote it, and is never told of it. For a guest's reply it is the
	// principal that posted it, under the guest's name.
	By *dgraphStruct.DgraphUser
	// Post is the thread's post, with its author (PostBy) and its channel.
	Post        *dgraphStruct.DgraphPost
	CommentUUID string
	// HTML is the reply as the Activity list shows it; PlainText, as push and
	// email do.
	HTML, PlainText string
	// Mentioned are those the reply mentions, as resolved when it was written.
	Mentioned []*dgraphStruct.DgraphUser
	At        time.Time
}

// What a reply's notices read and the ways they go, and the thread's index
// (business/ThreadIndex, chat P2): seams for tests.
var (
	threadOfPost     = domain.GetDgraphPostCommentsInfoByUUID
	levelsInChannel  = channelNotificationDomain.GetLevelsInChannel
	recordReply      = threadIndex.Replied
	threadFollows    = threadIndex.Following
	deliverReply     = notificationBusiness.Deliver
	emailPostComment = notificationBusiness.DispatchPostComment
	publishToUser    = activityBusiness.PublishActivityToUser
	signedProfileURL = userBusiness.GetSignedProfileURL
)

// threadReplyAudience works out who hears of r (ThreadAudience): those who
// replied before are read from the thread, who follows it or turned it off
// from its index, and everyone's setting for the channel, which says who is
// in it, in one read.
func threadReplyAudience(ctx context.Context, r ThreadReply) (notificationBusiness.Audience, error) {
	thread, err := threadOfPost(ctx, r.Post.Uuid)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	var repliers []string
	for _, c := range thread.Comments {
		if c != nil && c.CommentBy != nil {
			repliers = append(repliers, c.CommentBy.Uuid)
		}
	}
	mentioned := dgraphStruct.UUIDsOf(r.Mentioned)
	on, off, err := threadFollows(ctx, r.Post.Uuid)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	// Each person is asked about once, and only by a well-formed id: one
	// malformed id would fail the whole read, and tell nobody.
	var ask []string
	for _, id := range slices.Concat([]string{r.Post.PostBy.Uuid}, repliers, on, mentioned) {
		if _, err := uuid.Parse(id); err == nil && !slices.Contains(ask, id) {
			ask = append(ask, id)
		}
	}
	levels, err := levelsInChannel(ctx, r.Post.Channel.Uuid, ask)
	if err != nil {
		return notificationBusiness.Audience{}, err
	}
	return notificationBusiness.ThreadAudience(levels, r.By.Uuid, r.Post.PostBy.Uuid, repliers, mentioned,
		notificationBusiness.Follows{On: on, Off: off}), nil
}

// NotifyThreadReply records a reply in its thread's index (business/ThreadIndex)
// and tells the thread's people of it: a push, an email and,
// to the post's author and those mentioned, the live Activity item, all to the
// one audience worked out from the channel's members now (threadReplyAudience).
// When that can't be worked out, nobody is told: a missed notice costs less
// than the reply's text in the wrong inbox. It takes its time; run it on its
// own goroutine.
func NotifyThreadReply(r ThreadReply) {
	if r.By == nil || r.Post == nil || r.Post.PostBy == nil || r.Post.Channel == nil {
		return
	}
	ctx := context.Background()
	// The thread's index first: its latest reply, the writer and those
	// mentioned as its people, so the Threads view has it and the audience
	// counts who follows it now.
	recordReply(ctx, threadIndex.Reply{
		Thread: threadIndex.OfPost(r.Post), By: r.By.Uuid, Mentioned: dgraphStruct.UUIDsOf(r.Mentioned), At: r.At,
	})
	aud, err := threadReplyAudience(ctx, r)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NotifyThreadReply Failed to work out who hears of the reply, telling nobody err: %+v", err)
		return
	}
	if len(aud.Told) == 0 {
		return
	}
	icon := signedProfileURL(ctx, r.By.ProfileKey)

	// The push, and the desktop app's notice, to those of them the push gate
	// lets through (business/Notification Deliver). A failure here costs the
	// push only, never the email.
	pushData := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   r.Post.Channel.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:     "Comment - " + r.By.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_BODY:      r.PlainText,
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: r.Post.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME:  r.By.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_ICON:      icon,
	}
	deliverReply(ctx, aud.Told, pushData, "post_comment:"+r.Post.Uuid+":"+r.CommentUUID)

	// Email, by each one's own email settings (business/Notification).
	emailPostComment(r.By.Uuid, r.By.DisplayName(), icon,
		r.Post.Channel.Uuid, r.Post.Uuid, r.PlainText, r.CommentUUID, aud.Told)

	// The live Activity items.
	comment := func() *dgraphStruct.DgraphComment {
		at := r.At
		return &dgraphStruct.DgraphComment{
			Uuid:      r.CommentUUID,
			Text:      r.HTML,
			CommentBy: &dgraphStruct.DgraphUser{Uuid: r.By.Uuid, UserName: r.By.DisplayName()},
			Post:      &dgraphStruct.DgraphPost{Uuid: r.Post.Uuid, Channel: &dgraphStruct.DgraphChannel{Uuid: r.Post.Channel.Uuid}},
			CreatedAt: &at,
		}
	}
	if aud.Author != "" {
		publishToUser(aud.Author, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}
	for _, id := range aud.Mentioned {
		at := r.At
		publishToUser(id, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Mention:      &dgraphStruct.DgraphMentions{CommentUuid: r.CommentUUID, Comment: comment(), CreatedAt: &at},
		})
	}
}
