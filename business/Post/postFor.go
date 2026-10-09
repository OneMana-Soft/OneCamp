package business

// Telling a channel about a message written by someone who isn't a member.
//
// A channel guest's message (business/Guest) is posted by the "Guests"
// principal through botpost, the path every automated message takes, and
// botpost tells nobody: rightly for agents and automations, whose messages must
// not wake a channel. A guest is a person writing to the team, though, and
// nobody heard of it until they happened to open the channel. These tell the
// channel the way a member's message does, through the same functions, under
// the guest's name.

import (
	"fmt"
	"time"

	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// principalFor is a principal as a notification shows it: its own ids, so it
// is never told of its own words, under the name of whom it wrote for.
func principalFor(bot *userBusiness.BotIdentity, name string) *dgraphStruct.DgraphUser {
	u := &dgraphStruct.DgraphUser{Uid: bot.DgraphUID, Uuid: bot.UUID, UserName: name, IsBot: true}
	if bot.ProfileKey != "" {
		key := bot.ProfileKey
		u.ProfileKey = &key
	}
	return u
}

// NotifyPostFor tells a channel's members about a post a principal wrote for
// someone (name, as the notification shows them), the way CreatePost tells
// them of a member's: each member's setting for the channel decides.
func NotifyPostFor(bot *userBusiness.BotIdentity, name string, channel *dgraphStruct.DgraphChannel, postUUID, plainText string) {
	if bot == nil || channel == nil || channel.Uuid == "" {
		return
	}
	go sendNewPostNotification(fmt.Sprintf("#%s - %s", channel.Name, name), plainText, channel.Uuid, nil,
		channel, postUUID, principalFor(bot, name), "")
}

// NotifyReplyFor is NotifyPostFor for a reply in a thread: the post's author
// and those who replied before hear of it, as they do of a member's reply.
// bodyHTML is the reply as the activity list shows it, already safe to render.
func NotifyReplyFor(bot *userBusiness.BotIdentity, name string, post *dgraphStruct.DgraphPost, commentUUID, bodyHTML, plainText string) {
	if bot == nil || post == nil || post.PostBy == nil || post.Channel == nil {
		return
	}
	by := principalFor(bot, name)
	go commentBusiness.NotifyPostCommentFor(by, post, commentUUID, plainText)
	go PublishPostCommentActivity(commentUUID, bodyHTML, post.Uuid, time.Now(), nil, post, by)
}
