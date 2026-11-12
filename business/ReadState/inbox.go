package business

// The Inbox as a whole: opening it (POST /activity/seen) is a move of the
// person's read state like any other, told on read.moved to their own
// activity topic, so every tab and device they have open reads its badge
// again.

import (
	"context"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// KindInbox is read.moved's kind for the Inbox.
const KindInbox = "inbox"

// Seams for tests: the last look's write and the badge's count.
var (
	markInboxSeen = activityBusiness.MarkActivitySeen
	inboxUnread   = activityBusiness.UnreadActivityCount
)

// InboxSeen is who opening their Inbox: everything in it until now is seen
// (their last look moves), and it answers the badge after that write, what
// has arrived since, or nil when that can't be counted (the app asks again; it
// never shows a 0 the server didn't count). Their own activity topic hears of
// it (read.moved, kind inbox, with when, and the count when there is one), so
// every tab and device they have open reads its badge again. Only theirs:
// nobody else's last look moves, and nobody else hears. A last look that
// can't be written is the error, and tells nobody.
func InboxSeen(ctx context.Context, who *userModels.UserInfo) (*uint64, error) {
	if who == nil {
		return nil, conversationBusiness.ErrNotFound
	}
	if err := markInboxSeen(ctx, who.UserPostgresInfo.Id); err != nil {
		return nil, err
	}
	ev := MovedEvent{Kind: KindInbox, Seen: time.Now().UTC().Format(time.RFC3339Nano)}
	var count *uint64
	if n, err := inboxUnread(ctx, who.UserPostgresInfo.Id, who.UserDgraphInfo.Uid); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ReadState InboxSeen: %s's badge couldn't be counted after the write: %+v", who.UserPostgresInfo.Id, err)
	} else {
		count, ev.Unread = &n, int(n)
	}
	if who.UserDgraphInfo.Uuid != "" {
		publish(who.UserDgraphInfo.Uuid, mqttStruct.MESSAGE_READ_MOVED, ev)
	}
	return count, nil
}
