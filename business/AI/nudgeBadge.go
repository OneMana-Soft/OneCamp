package business

// The Inbox badge counts a person's urgent OneCamp AI nudges by the rule it
// counts everything by (business/Activity unread.go): one raised since their
// last look at the Inbox is unread, and opening the Inbox clears it from the
// badge, though it stays listed until it is resolved or dismissed. The web
// added them to the badge on its own, outside that rule, so a five-month-old
// "Overdue commitment" kept the owner's badge at 1 after every look (V-577).

import (
	"context"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func init() { activityBusiness.UrgentNudges = urgentNudges }

// Seams for tests: whether OneCamp AI is on (the probe the app's "ai" feature
// reads, so the web lists nudges exactly when this counts them) and the raise
// times of a person's open urgent nudges.
var (
	aiOn        = func() bool { return ai.GetService().IsEnabled() }
	urgentRaise = nudgeModels.UrgentRaisedFor
)

// nudgeItem is a nudge as the badge's rule reads it: an item with no
// conversation, at the time it was last raised.
func nudgeItem(raised time.Time) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "NUDGE", Time: raised.UTC().Format(time.RFC3339Nano)}
}

// urgentNudges is activityBusiness.UrgentNudges: the person's own open urgent
// nudges, at most limit, or none while OneCamp AI is off, when the Inbox lists
// none.
func urgentNudges(ctx context.Context, user uuid.UUID, limit int) ([]dgraphModels.UnifiedActivityItem, error) {
	if !aiOn() {
		return nil, nil
	}
	raised, err := urgentRaise(ctx, user, limit)
	if err != nil {
		return nil, err
	}
	items := make([]dgraphModels.UnifiedActivityItem, len(raised))
	for i, at := range raised {
		items[i] = nudgeItem(at)
	}
	return items, nil
}

// MarkNudgesUnread sets Unread on the person's listed nudges as the badge
// counts them: an urgent one raised since their last look. A nudge at
// priority 0 is listed and never counted, so it has no dot. A last look that
// can't be read marks none, as with the feed.
func MarkNudgesUnread(ctx context.Context, user uuid.UUID, nudges []*nudgeModels.Nudge) error {
	var urgent []*nudgeModels.Nudge
	var items []dgraphModels.UnifiedActivityItem
	for _, n := range nudges {
		n.Unread = false
		if n.Priority > 0 {
			urgent = append(urgent, n)
			items = append(items, nudgeItem(raisedAt(n)))
		}
	}
	if len(items) == 0 {
		return nil
	}
	if err := activityBusiness.MarkUnread(ctx, user, items); err != nil {
		return err
	}
	for i, n := range urgent {
		n.Unread = items[i].Unread
	}
	return nil
}

// raisedAt is when n was last raised, as UrgentRaisedFor reads it.
func raisedAt(n *nudgeModels.Nudge) time.Time {
	if n.UpdatedAt.After(n.CreatedAt) {
		return n.UpdatedAt
	}
	return n.CreatedAt
}
