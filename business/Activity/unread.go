package business

// What the Inbox badge counts and the dots the Inbox draws: one rule
// (unreadInInbox) over one set of items, the Inbox's default view (the feed
// with its notices, after keepReadable, and the person's urgent OneCamp AI
// nudges at the top of it), so the badge never counts an item the Inbox
// doesn't mark, and the Inbox marks every item the badge counted.

import (
	"context"
	"time"

	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	lastSeenActivityDomain "github.com/akashc777/OneCamp/domain/LastSeenActivity"
	lastSeenChannelDomain "github.com/akashc777/OneCamp/domain/LastSeenChannel"
	lastSeenChatDomain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	"github.com/google/uuid"
)

// looks is what the person has looked at: their Inbox as a whole (the last
// look, which only POST /activity/seen moves) and each conversation they have
// read (its ReadState marker, the last_seen_channel and last_seen_chat rows
// that reading it moves: business/ReadState).
type looks struct {
	inbox    time.Time
	channels map[string]time.Time
	chats    map[string]time.Time
}

// engaged reports whether they have read item's conversation at or after the
// item: its channel, or its DM or group, by the conversation's marker. An
// item with no conversation (a reaction, a notice, a mention on a task) has
// none to read it in.
func (l looks) engaged(item *dgraphModels.UnifiedActivityItem) bool {
	kind, id := activityScope(item)
	if id == "" {
		return false
	}
	at := parseActivityTime(item.Time)
	if at.IsZero() {
		return false
	}
	var seen time.Time
	var ok bool
	switch kind {
	case "channel":
		seen, ok = l.channels[id]
	case "chat":
		seen, ok = l.chats[id]
	}
	return ok && !seen.Before(at)
}

// unreadInInbox is the one rule the badge counts by and the Inbox draws its
// dots by: the item is newer than the person's last look at their Inbox, and
// they haven't read it where it is since (its conversation's marker at or
// after it: looks.engaged). Which items it is asked of is the
// Inbox's default view, the feed with its notices after keepReadable, so it
// never counts what they can't open. Pure.
func unreadInInbox(item *dgraphModels.UnifiedActivityItem, l looks) bool {
	at, err := time.Parse(activityTimeLayout, item.Time)
	return err == nil && at.After(l.inbox) && !l.engaged(item)
}

// markUnread sets each item's Unread by unreadInInbox and answers how many
// are.
func markUnread(items []dgraphModels.UnifiedActivityItem, l looks) int {
	n := 0
	for i := range items {
		items[i].Unread = unreadInInbox(&items[i], l)
		if items[i].Unread {
			n++
		}
	}
	return n
}

// inConversation reports whether any of items is in a channel or a
// conversation, so has a marker to read: a page of reactions and notices
// needs none, and costs no read.
func inConversation(items []dgraphModels.UnifiedActivityItem) bool {
	for i := range items {
		if _, id := activityScope(&items[i]); id != "" {
			return true
		}
	}
	return false
}

// Seams for the badge's tests: the stored count, the last look, the
// conversations' markers, and the feed's page.
var (
	storedUnread = lastSeenActivityBusiness.GetTotalUnreadActivityCount
	lastLookOf   = lastSeenActivityDomain.GetLastSeenActivityByUserId
	markersOf    = func(ctx context.Context, user uuid.UUID) (channels, chats map[string]time.Time, err error) {
		channels, err = lastSeenChannelDomain.GetAllLastSeenChannelsForUser(ctx, user)
		if err != nil {
			return nil, nil, err
		}
		chats, err = lastSeenChatDomain.GetAllLastSeenChatsForUser(ctx, user)
		if err != nil {
			// The channels' markers still say what they read there.
			helpers.LogErrorWithContext(ctx, "business/Activity: %s's conversation markers couldn't be read: %+v", user, err)
			return channels, nil, nil
		}
		return channels, chats, nil
	}
	pageOf = feedPage
)

// readLooks reads what the person has looked at, as far as items need: the
// conversations' markers only when one of them is in a conversation. Markers
// that can't be read are logged and left out (nothing counts as read in its
// conversation); a last look that can't be read is the error.
func readLooks(ctx context.Context, user uuid.UUID, items []dgraphModels.UnifiedActivityItem) (looks, error) {
	var l looks
	if inConversation(items) {
		channels, chats, err := markersOf(ctx, user)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Activity: %s's channel markers couldn't be read: %+v", user, err)
		}
		l.channels, l.chats = channels, chats
	}
	last, err := lastLookOf(ctx, user)
	if err != nil {
		return l, err
	}
	l.inbox = last
	return l, nil
}

// NoticeTimes reads the same notices as NoticeSource, bare: each item's kind
// and time, with none of the titles, people or access reads the Inbox draws
// them with, which the badge has no use for. business/Follow sets it from its
// init; until then there are none.
var NoticeTimes func(ctx context.Context, userUUID string, before time.Time, limit int) ([]dgraphModels.UnifiedActivityItem, error)

// UrgentNudges reads the person's own open urgent OneCamp AI nudges, bare:
// each an item of type NUDGE at the time it was last raised (made, or raised
// again), which is all the badge needs of it. business/AI sets it from its
// init, and answers none while OneCamp AI is off, as the Inbox lists none
// then; the edition without AI links no AI package, so it stays nil there and
// the badge counts no nudges.
var UrgentNudges func(ctx context.Context, user uuid.UUID, limit int) ([]dgraphModels.UnifiedActivityItem, error)

// MarkUnread sets Unread on items the Inbox lists beside the feed (the
// person's OneCamp AI nudges) by the one rule, unreadInInbox, so it draws
// their dots on exactly what the badge counts. A last look that can't be read
// is the error, and marks nothing, as with the feed.
func MarkUnread(ctx context.Context, user uuid.UUID, items []dgraphModels.UnifiedActivityItem) error {
	l, err := readLooks(ctx, user, items)
	if err != nil {
		return err
	}
	markUnread(items, l)
	return nil
}
