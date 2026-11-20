package business

// A4 H5: Priority is triage the person can clear. An ask lets go of Priority
// a week after it was asked, and the person can take an item out of their
// own Priority and put it back (POST /activity/dismiss; models/postgres
// InboxMark). Only Priority changes: the item stays in the feed, its badge,
// its conversation and search, and nobody else's Priority moves.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	inboxMarkModels "github.com/akashc777/OneCamp/models/postgres/InboxMark"
	"github.com/google/uuid"
)

const (
	// priorityLetsGoAfter is how long an ask stays in Priority unanswered.
	priorityLetsGoAfter = 7 * 24 * time.Hour
	// maxDismissKeys is the most keys one dismissal takes: a page of the
	// feed's collapsed echoes is far fewer.
	maxDismissKeys = 20
	// maxKeyBytes is inbox_marks.item_key's limit.
	maxKeyBytes = 200
)

// ErrBadKey is a dismissal that names no Priority item: no keys, too many,
// or one that isn't a key the feed sends.
var ErrBadKey = errors.New("not a Priority item's key")

// Seams for tests: the person's dismissals, written and read.
var (
	markDismissed  = inboxMarkModels.SetDismissed
	readDismissals = inboxMarkModels.Dismissed
)

// itemKey is the server's one name for a feed item that may be Priority: its
// kind and its record's own id ("MENTION|id|<uid>", "COMMENT|id|<uuid>",
// "NOTICE|id|<id>"), or "" for a reaction, which is never Priority, and for
// an item with no id. The feed sends it on the item (Key); the app posts it
// back as it is, and it is only ever matched inside the person's own feed.
func itemKey(item *dgraphModels.UnifiedActivityItem) string {
	id := ""
	switch item.ActivityType {
	case "MENTION":
		if item.Mention != nil {
			id = item.Mention.Uid
		}
	case "COMMENT":
		if item.Comment != nil {
			id = item.Comment.Uuid
		}
	case "NOTICE":
		if item.Notice != nil {
			id = item.Notice.ID
		}
	}
	if id == "" {
		return ""
	}
	return item.ActivityType + "|id|" + id
}

// checkKeys refuses (ErrBadKey) a dismissal of no keys, of more than
// maxDismissKeys, or of a key that isn't one itemKey makes.
func checkKeys(keys []string) error {
	if len(keys) == 0 || len(keys) > maxDismissKeys {
		return ErrBadKey
	}
	for _, k := range keys {
		kind, id, ok := strings.Cut(k, "|id|")
		if !ok || id == "" || len(k) > maxKeyBytes || !utf8.ValidString(k) ||
			strings.IndexFunc(k, unicode.IsControl) >= 0 ||
			(kind != "MENTION" && kind != "COMMENT" && kind != "NOTICE") {
			return ErrBadKey
		}
	}
	return nil
}

// Dismiss takes the person's items out of their Priority (dismissed) or puts
// them back, by the keys their feed sent. The person is the caller's own
// session: a key is never resolved to anything, only matched inside their
// own feed as it is read, so a key for something they can't see changes
// nothing they or anyone else is shown.
func Dismiss(ctx context.Context, user uuid.UUID, keys []string, dismissed bool) error {
	if err := checkKeys(keys); err != nil {
		return err
	}
	return markDismissed(ctx, user, keys, dismissed)
}

// keyItems names each item of a page by itemKey.
func keyItems(items []dgraphModels.UnifiedActivityItem) {
	for i := range items {
		items[i].Key = itemKey(&items[i])
	}
}

// dismissalsOf is which of the page's high items the person took out of
// Priority, read once. Best-effort: a read failure is logged and dismisses
// nothing, and the week still lets old asks go.
func dismissalsOf(ctx context.Context, user uuid.UUID, items []dgraphModels.UnifiedActivityItem) map[string]bool {
	var keys []string
	for i := range items {
		if items[i].Priority == PriorityHigh && items[i].Key != "" {
			keys = append(keys, items[i].Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	d, err := readDismissals(ctx, user, keys)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetUnifiedActivity: %s's Priority dismissals couldn't be read, so none applies: %+v", user, err)
		return nil
	}
	return d
}

// clearFromPriority lets go of what Priority holds (high) and the person no
// longer needs to see there: an ask over a week old, or one they dismissed
// (marked Dismissed, so the app offers to put it back). Either reads normal,
// never low: the Inbox reads a counted item that drops to low as read, so
// low would clear its dot in All too. Run after demoteAnswered: an answered
// ask is low already and stays so. An item whose time can't be read keeps
// its priority.
func clearFromPriority(items []dgraphModels.UnifiedActivityItem, dismissed map[string]bool, now time.Time) {
	for i := range items {
		it := &items[i]
		if it.Priority != PriorityHigh {
			continue
		}
		if at := parseActivityTime(it.Time); !at.IsZero() && now.Sub(at) > priorityLetsGoAfter {
			it.Priority = PriorityNormal
			continue
		}
		if it.Key != "" && dismissed[it.Key] {
			it.Priority, it.Dismissed = PriorityNormal, true
		}
	}
}
