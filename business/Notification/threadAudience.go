package notification

// Who hears of a reply in a thread: a channel's (business/Comment
// NotifyThreadReply) or a DM's or group's (business/Chat). Each of them works
// out one audience here, and push, the desktop notice, the live Activity items
// and email all go to it. A DM or group reply used to be told by two lists:
// push to everyone in the conversation on "all", Activity and email to the
// message's author and those mentioned.

import (
	"strings"

	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
)

// Audience is who hears of a reply.
type Audience struct {
	// Told hears of it by push, desktop notice and email: each person once,
	// the message's author first, then those who replied before, then those
	// following the thread, then those mentioned.
	Told []string
	// Author is the message's author when told: they get the live "replied
	// to your message" item.
	Author string
	// Mentioned are those mentioned who are told, the author aside: they get
	// the live mention item.
	Mentioned []string
}

// Follows is who chose, for one thread, to hear of each of its replies
// whatever their part in it (On), and who chose to hear of none (Off), as the
// thread's index keeps it (business/ThreadIndex Following, chat P2).
type Follows struct {
	On, Off []string
}

// ThreadAudience is who hears of a reply: of the message's author, those who
// replied before, those following the thread and those the reply mentions,
// never its writer, the ones in the conversation now, each as their setting
// for it allows. levels is each candidate's setting by lower-case user id;
// someone with none isn't in the conversation and hears nothing. A thread is
// the people in it, so "all" and "mention" ("Mentions and threads you're in")
// hear as any of them; "block" ("Nothing") hears only what is addressed to
// them, as the author or someone mentioned, and not every reply in a thread
// they answered once or follow. Someone who turned the thread off hears only
// a reply that mentions them, even their own message's. Pure.
func ThreadAudience(levels map[string]string, writer, author string, repliers, mentioned []string, follows Follows) Audience {
	key := func(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
	isMentioned := map[string]bool{}
	for _, id := range mentioned {
		isMentioned[key(id)] = true
	}
	addressed := func(id string) bool { return key(id) == key(author) || isMentioned[key(id)] }
	off := map[string]bool{}
	for _, id := range follows.Off {
		off[key(id)] = true
	}

	var aud Audience
	seen := map[string]bool{"": true, key(writer): true}
	consider := func(id string) {
		k := key(id)
		if seen[k] {
			return
		}
		seen[k] = true
		if off[k] && !isMentioned[k] {
			return
		}
		switch levels[k] {
		case postgressStruct.NOTIFICATION_TYPE_ALL, postgressStruct.NOTIFICATION_TYPE_MENTION:
		case postgressStruct.NOTIFICATION_TYPE_BLOCK:
			if !addressed(id) {
				return
			}
		default: // not in the conversation
			return
		}
		aud.Told = append(aud.Told, id)
		switch {
		case k == key(author):
			aud.Author = id
		case isMentioned[k]:
			aud.Mentioned = append(aud.Mentioned, id)
		}
	}
	consider(author)
	for _, id := range repliers {
		consider(id)
	}
	for _, id := range follows.On {
		consider(id)
	}
	for _, id := range mentioned {
		consider(id)
	}
	return aud
}
