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

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
)

// chatSettingsRepairTries bounds the retries in one process (Postgres may
// still be starting); the next start tries again.
const chatSettingsRepairTries = 8

// StartChatSettingsRepair gives everyone in a conversation without a setting
// for it the default (domain.GiveMissingChatSettings), in the background,
// until a run finishes, ctx ends, or the tries run out. It never delays or
// fails a start.
func StartChatSettingsRepair(ctx context.Context) {
	helpers.GoSafeNamed("chat.settings-repair", func() {
		wait := 5 * time.Second
		for try := 0; try < chatSettingsRepairTries; try++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			given, err := domain.GiveMissingChatSettings(ctx)
			if given > 0 {
				helpers.MessageLogs.InfoLog.Printf("chat settings: gave %d people in a conversation without a notification setting the default", given)
			}
			if err == nil {
				return
			}
			if wait < 10*time.Minute {
				wait *= 2
			}
		}
	})
}
