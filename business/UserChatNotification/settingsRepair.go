package business

// Conversations whose people had no notification setting for them.
//
// Who hears of a direct or group message is read from users_chat_notification
// alone, and two paths left people without a row there: adding someone to a
// group moved it to a new id and left the settings under the old one, and a
// new direct conversation's two rows were one insert that failed whole when
// either row was already there. Nobody without a row heard of anything in the
// conversation again: no push, email, Activity item or call ring. Both paths
// are mended where they write; this gives everyone already left without a
// setting the default, at every start, so installs from before the fix come
// back to life. When nobody is missing one, a run is one query that inserts
// nothing.
//
// A setting is nobody's place in a conversation, though: someone who left a
// group kept theirs and was told of the group's messages. Each run first takes
// away the setting of everyone who has left the group it is for
// (domain.ForgetLeaversChatSettings), so installs from before Leave did that
// itself are cleaned without anyone acting, and it gives nobody who left a
// group a setting for it back.

import (
	"context"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	domain "github.com/akashc777/OneCamp/domain/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
)

// What the repair reads and takes away: seams for tests.
var (
	indexState       = conversationDomain.StateNow
	forgetLeaversNow = domain.ForgetLeaversChatSettings
)

// forgetLeavers takes away the settings of everyone the conversations index has
// as having left the group they are for (domain.ForgetLeaversChatSettings),
// unless the index is marked dirty: a write to it failed, and one that failed
// while putting a leaver back, the graph having kept them, leaves the index
// saying a member left. A later start does it, once a comparison has repaired
// the index; meanwhile nobody out of a group is told of it
// (InConversationNow). A state that can't be read is an error, to try again.
func forgetLeavers(ctx context.Context) (int64, error) {
	v, err := indexState(ctx)
	if err != nil {
		return 0, err
	}
	if s, ok := conversationDomain.ParseState(v); ok && s.Kind == conversationDomain.StateDirty {
		helpers.LogInfoWithContext(ctx, "chat settings: the conversations index is marked dirty (%s): the settings of people who left a group are taken away at a later start", s.Note)
		return 0, nil
	}
	return forgetLeaversNow(ctx)
}

// chatSettingsRepairTries bounds the retries in one process (Postgres may
// still be starting); the next start tries again.
const chatSettingsRepairTries = 8

// StartChatSettingsRepair takes away the settings of everyone who has left the
// group they are for (forgetLeavers), then gives everyone in a conversation
// without a setting for it the default (domain.GiveMissingChatSettings), in the
// background, until a run finishes, ctx ends, or the tries run out. It never
// delays or fails a start.
func StartChatSettingsRepair(ctx context.Context) {
	helpers.RetryInBackground(ctx, "chat.settings-repair",
		helpers.Retries{First: 5 * time.Second, Cap: 10 * time.Minute, Tries: chatSettingsRepairTries},
		func(ctx context.Context) error {
			forgot, err := forgetLeavers(ctx)
			if forgot > 0 {
				helpers.MessageLogs.InfoLog.Printf("chat settings: took away %d settings of people who have left the group they were for", forgot)
			}
			if err != nil {
				return err
			}
			given, err := domain.GiveMissingChatSettings(ctx)
			if given > 0 {
				helpers.MessageLogs.InfoLog.Printf("chat settings: gave %d people in a conversation without a notification setting the default", given)
			}
			return err
		})
}
