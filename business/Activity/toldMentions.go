package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

// Seam for tests: which notice keys the inbox holds.
var heldNotices = func(ctx context.Context, keys []string) (map[string]bool, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return map[string]bool{}, nil
	}
	return followModels.HeldNotices(ctx, keys)
}

// dropToldMentions leaves out each mention of the reader that they were told
// of by a notice (business/Notification TellMentionsAdded): an edit named
// them after the message was first saved, so the graph's mention keeps the
// first save's time, and the notice, at the edit, lists it. Each mention is
// listed and counted once, at the edit (NOTIF-H3). One read by the notices'
// unique key, and only for a page with mentions in it; a read that fails
// leaves the page as it was.
func dropToldMentions(ctx context.Context, reader string, items []dgraphModels.UnifiedActivityItem) []dgraphModels.UnifiedActivityItem {
	var keys []string
	for _, it := range items {
		if k := toldMentionKey(it, reader); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return items
	}
	held, err := heldNotices(ctx, keys)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Activity dropToldMentions: %s's notices couldn't be read, listing the graph's mentions: %+v", reader, err)
		return items
	}
	if len(held) == 0 {
		return items
	}
	kept := make([]dgraphModels.UnifiedActivityItem, 0, len(items))
	for _, it := range items {
		if !held[toldMentionKey(it, reader)] {
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
