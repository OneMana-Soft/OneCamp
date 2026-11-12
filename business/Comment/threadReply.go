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
// Now the Notification Router (business/Notify, A4 H2 "One door for chat")
// plans it, by the thread's rule (notificationBusiness.ThreadAudience, which
// DM and group threads use too) over the channel's members as they are when
// the reply is written, and records each delivery once; push, email and the
// live Activity items all go to whom it told. The router can't be imported
// here (it reads business/Reach, which reads this package), so business/Post
// gives it at init (RouteRepliesWith); until then nobody is told.

import (
	"context"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
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
	// Outsider is a writer outside the conversation (let into its call by the
	// call's invite, calls CALLS-3b): the thread's index records the reply but
	// not them, so the thread never enters their Threads.
	Outsider bool
}

// replyRecord is the reply as the thread's index records it: its writer, unless
// they're outside the conversation, and those it mentions. Pure.
func replyRecord(r ThreadReply) threadIndex.Reply {
	by := r.By.Uuid
	if r.Outsider {
		by = ""
	}
	return threadIndex.Reply{Thread: threadIndex.OfPost(r.Post), By: by, Mentioned: dgraphStruct.UUIDsOf(r.Mentioned), At: r.At}
}

// What a reply's notices read and the ways they go, and the thread's index
// (business/ThreadIndex, chat P2): seams for tests.
var (
	threadOfPost     = domain.GetDgraphPostCommentsInfoByUUID
	recordReply      = threadIndex.Replied
	emailPostComment = notificationBusiness.DispatchPostComment
	publishToUser    = activityBusiness.PublishActivityToUser
	signedProfileURL = userBusiness.GetSignedProfileURL
)

// ReplyNotice is a reply in a channel thread as the router plans it: an event
// with cause message in the thread of PostID in ChannelID, by Actor, answering
// Author, with those who replied before and those it mentions. ID names it
// (one row per person and delivery), MessageID is the reply's own id (its
// delete retracts it), Push is its push.
type ReplyNotice struct {
	ID, MessageID      string
	Actor, Author      string
	ChannelID, PostID  string
	Repliers, Mentions []string
	Push               map[string]string
}

// ReplyTold is whom the router told of a reply, newly: Thread is who the
// thread's rule told (the post's author, those mentioned, those who replied
// before or follow it), the email's people; RepliedTo is the post's author
// told of it as an answer to them, Mentioned those told of it as a mention:
// the live Activity items' people.
type ReplyTold struct {
	Thread, RepliedTo, Mentioned []string
}

// The router's notice of a reply (business/Notify Tell) and its Inbox answer
// for one person (Explain): given by business/Post at init.
var (
	tellReply    = func(context.Context, ReplyNotice) ReplyTold { return ReplyTold{} }
	inReplyInbox = func(context.Context, ReplyNotice, string) bool { return false }
)

// RouteRepliesWith gives this package the router: tell is Plan then Deliver
// for a reply's notice, answering whom it told; inbox says whether the plan
// puts the reply in who's Inbox as an answer to them or a mention.
func RouteRepliesWith(tell func(ctx context.Context, n ReplyNotice) ReplyTold, inbox func(ctx context.Context, n ReplyNotice, who string) bool) {
	tellReply, inReplyInbox = tell, inbox
}

// replyNotice is r as the router plans it: those who replied before are read
// from the thread (who follows it, or turned it off, the router reads from
// its index, and everyone's setting for the channel).
func replyNotice(ctx context.Context, r ThreadReply) (ReplyNotice, error) {
	thread, err := threadOfPost(ctx, r.Post.Uuid)
	if err != nil {
		return ReplyNotice{}, err
	}
	var repliers []string
	for _, c := range thread.Comments {
		if c != nil && c.CommentBy != nil {
			repliers = append(repliers, c.CommentBy.Uuid)
		}
	}
	return ReplyNotice{Actor: r.By.Uuid, Author: r.Post.PostBy.Uuid, ChannelID: r.Post.Channel.Uuid, PostID: r.Post.Uuid,
		Repliers: repliers, Mentions: dgraphStruct.UUIDsOf(r.Mentioned)}, nil
}

// InThreadInbox says whether telling r puts it in who's Inbox, counted: the
// live Activity item NotifyThreadReply sends the post's author and those r
// mentions, by the router's plan for who alone (business/Notify Explain).
// False when it can't be worked out.
func InThreadInbox(ctx context.Context, r ThreadReply, who string) bool {
	if r.By == nil || r.Post == nil || r.Post.PostBy == nil || r.Post.Channel == nil || who == "" {
		return false
	}
	n, err := replyNotice(ctx, r)
	if err != nil {
		return false
	}
	return inReplyInbox(ctx, n, who)
}

// NotifyThreadReply records a reply in its thread's index (business/ThreadIndex)
// and tells the thread's people of it through the one door (tellReply): the
// push and the desktop notice, each delivery recorded once; then the email
// and, to the post's author and those mentioned, the live Activity item, to
// whom it told. When the thread can't be read, nobody is told: a missed
// notice costs less than the reply's text in the wrong inbox. It takes its
// time; run it on its own goroutine.
func NotifyThreadReply(r ThreadReply) {
	if r.By == nil || r.Post == nil || r.Post.PostBy == nil || r.Post.Channel == nil {
		return
	}
	ctx := context.Background()
	// The thread's index first: its latest reply, the writer and those
	// mentioned as its people, so the Threads view has it and the plan
	// counts who follows it now.
	recordReply(ctx, replyRecord(r))
	n, err := replyNotice(ctx, r)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NotifyThreadReply Failed to read the thread, telling nobody err: %+v", err)
		return
	}
	icon := signedProfileURL(ctx, r.By.ProfileKey)
	n.ID, n.MessageID = "post_comment:"+r.Post.Uuid+":"+r.CommentUUID, r.CommentUUID
	n.Push = map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   r.Post.Channel.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:     "Comment - " + r.By.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_BODY:      r.PlainText,
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: r.Post.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME:  r.By.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_ICON:      icon,
	}
	told := tellReply(ctx, n)

	// Email, by each one's own email settings (business/Notification).
	if len(told.Thread) > 0 {
		emailPostComment(r.By.Uuid, r.By.DisplayName(), icon,
			r.Post.Channel.Uuid, r.Post.Uuid, r.PlainText, r.CommentUUID, told.Thread)
	}

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
	for _, id := range told.RepliedTo {
		publishToUser(id, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}
	for _, id := range told.Mentioned {
		at := r.At
		publishToUser(id, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Mention:      &dgraphStruct.DgraphMentions{CommentUuid: r.CommentUUID, Comment: comment(), CreatedAt: &at},
		})
	}
}
