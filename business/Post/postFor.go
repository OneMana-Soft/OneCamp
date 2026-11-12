package business

// Telling a channel about a message written by someone who isn't a member.
//
// A channel guest's message (business/Guest) is posted by the "Guests"
// principal through botpost, the path every automated message takes, and
// botpost tells nobody of a new post: rightly for agents and automations,
// whose posts must not wake a channel. A guest is a person writing to the
// team, though, and nobody heard of it until they happened to open the
// channel. This tells the channel the way a member's message does, through the
// same function, under the guest's name. A reply in a thread, a guest's or any
// bot's, is told as a member's is (botpost TellReply).

import (
	"fmt"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// NotifyPostFor tells a channel's members about a post a principal wrote for
// someone (name, as the notification shows them), the way CreatePost tells
// them of a member's: each member's setting for the channel decides.
func NotifyPostFor(bot *userBusiness.BotIdentity, name string, channel *dgraphStruct.DgraphChannel, postUUID, plainText string) {
	if bot == nil || channel == nil || channel.Uuid == "" {
		return
	}
	helpers.GoSafeNamed("sendNewPostNotification", func() {
		sendNewPostNotification(fmt.Sprintf("#%s - %s", channel.Name, name), plainText, channel.Uuid, nil,
			channel, postUUID, bot.Principal(name), "")
	})
}
