package business

import (
	"strings"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// keepHeard is items less those in a channel or conversation whose setting
// (the reader's own) doesn't let it tell them of them, by the one rule
// (notifylevel.Hears): a mention is Named, a comment in a thread they are in is
// Thread. An @channel or @here is a Thread to the push (business/Notify
// heardOf); every level lets a Thread and a Named alike, so it is told the
// same. The feed and the Inbox badge both count from feedPage, so a muted
// channel's mention is neither listed nor counted, and it stays readable in
// its channel: a setting only says whether they are told.
//
// channels and conversations are the reader's own settings for the page's
// places, each by lower-case id, from the page's one read (toldAndHeard). A
// place with no setting keeps its items (Hears: only a mute removes).
func keepHeard(items []dgraphModels.UnifiedActivityItem, channels, conversations map[string]string) []dgraphModels.UnifiedActivityItem {
	kept := make([]dgraphModels.UnifiedActivityItem, 0, len(items))
	for _, it := range items {
		kind, id := activityScope(&it)
		levels := conversations
		if kind == "channel" {
			levels = channels
		}
		heard := notifylevel.Thread
		if it.ActivityType == "MENTION" {
			heard = notifylevel.Named
		}
		if kind != "" && !notifylevel.Hears(levels, strings.ToLower(id), heard) {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}
