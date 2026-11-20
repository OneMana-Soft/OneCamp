package business

import (
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

// dropToldMentions leaves out each mention of the reader that they were told
// of by a notice (business/Notification TellMentionsAdded): an edit named
// them after the message was first saved, so the graph's mention keeps the
// first save's time, and the notice, at the edit, lists it. Each mention is
// listed and counted once, at the edit (NOTIF-H3). keys[i] is items[i]'s
// told-mention key (toldMentionKey), "" for an item that isn't a mention, and
// held is which keys name a notice the inbox shows, from the page's one read
// (toldAndHeard).
func dropToldMentions(items []dgraphModels.UnifiedActivityItem, keys []string, held map[string]bool) []dgraphModels.UnifiedActivityItem {
	if len(held) == 0 {
		return items
	}
	kept := make([]dgraphModels.UnifiedActivityItem, 0, len(items))
	for i, it := range items {
		if !held[keys[i]] {
			kept = append(kept, it)
		}
	}
	return kept
}

// toldMentionKey is the key the reader's notice of a mention item would have
// (followModels.MentionKey and their id), or "" for any other item. Pure.
func toldMentionKey(it dgraphModels.UnifiedActivityItem, reader string) string {
	m := it.Mention
	if m == nil {
		return ""
	}
	kind, id := "", ""
	switch {
	case m.Comment != nil && m.Comment.Uuid != "":
		kind, id = "comment", m.Comment.Uuid
	case m.Post != nil && m.Post.Uuid != "":
		kind, id = "post", m.Post.Uuid
	case m.Chat != nil && m.Chat.Uuid != "":
		kind, id = "chat", m.Chat.Uuid
	default:
		return ""
	}
	return followModels.MentionKey(kind, id) + "|" + reader
}
