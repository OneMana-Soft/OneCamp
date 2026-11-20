// Package notifylevel is what a member's setting for a channel, a DM or
// group, or a project lets them be told of. It is the one copy of the rule:
// every path that tells someone of something (push, desktop, sound, email,
// the Inbox and its badge, a thread's replies, Notify anyway) asks Lets, after
// reading the levels of everyone it would tell in one read.
//
// A setting is the member's own and only says whether they are told: it never
// hides what they may read (the message, the task, the thread, search).
//
// No setting hears (Hears): only a mute removes. Where a place's settings are
// also how it knows who is in it now (a channel's, a DM's or a group's, as
// business/Notify reads them), someone with none isn't there: that is the
// place's own read of who is in it, asked before this rule.
//
// One thing a level doesn't silence: a reaction to someone's own message
// still reaches its author's Activity on "Nothing", live and in the list,
// since it is theirs alone, as Slack keeps it (a REACTION item has no place
// for business/Activity keepHeard to ask). It is never more than that item:
// the router's delivery of it (Thread) asks Lets like any other, so no push,
// desktop notice, sound, email or Inbox row. Pinned by tests/integration
// TestOneDoorAReactionReachesAnAuthorOnNothing.
package notifylevel

import postgressStruct "github.com/akashc777/OneCamp/models/postgres"

// Heard is what a notice is to the person it would tell.
type Heard int

const (
	// Plain is a message for everyone in the place: a channel's or a DM's
	// message, a call, a project's update.
	Plain Heard = iota
	// Thread is something they are in: a reply in a thread they answered or
	// follow, a broadcast (@channel, @here, @everyone), a team they are in,
	// one of their keywords, a reaction to their message.
	Thread
	// Named is something for them: an @mention of them, a reply to their own
	// message, a task given to them or theirs, Notify anyway.
	Named
)

// Lets is whether a member's level for a place lets a notice through:
// "Everything" (all) every notice; "Mentions and threads you're in"
// (mention) all but a Plain one; "Nothing" (block) none, an @mention of them
// included. Any other value lets none. Someone with no setting at all is
// Hears'. Pure.
func Lets(level string, h Heard) bool {
	switch level {
	case postgressStruct.NOTIFICATION_TYPE_ALL:
		return true
	case postgressStruct.NOTIFICATION_TYPE_MENTION:
		return h != Plain
	}
	return false
}

// Hears is whether id hears what reaches them as h by their setting in levels
// (everyone's, read once, by the id as given): with a setting, as Lets says;
// with none, yes, since only a mute removes. Pure.
func Hears(levels map[string]string, id string, h Heard) bool {
	level, set := levels[id]
	return HearsAt(level, set, h)
}

// HearsAt is Hears for one person's setting, read apart: level when set, and
// with none, yes. Pure.
func HearsAt(level string, set bool, h Heard) bool {
	return !set || Lets(level, h)
}

// Hearing is which of ids hear (Hears) what reaches each of them as heard
// says, in the order given: levels is everyone's setting, read once, by the
// id as given.
func Hearing(levels map[string]string, ids []string, heard func(id string) Heard) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if Hears(levels, id, heard(id)) {
			out = append(out, id)
		}
	}
	return out
}
