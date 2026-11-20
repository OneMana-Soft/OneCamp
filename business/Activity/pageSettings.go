package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Activity"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// pageSettings reads what a page of reader's Inbox asks of Postgres
// (domain.ReaderPageSettings). A seam for tests; with no database nothing is
// held and no place has a setting, so the page is kept.
var pageSettings = func(ctx context.Context, reader string, keys, channelIDs, grpIDs []string) (domain.PageSettings, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return domain.PageSettings{}, nil
	}
	return domain.ReaderPageSettings(ctx, reader, keys, channelIDs, grpIDs)
}

// toldAndHeard is a page of reader's feed less the mentions told by a notice
// (dropToldMentions), then less what their setting for its channel or
// conversation doesn't let it tell them of (keepHeard), both from ONE read of
// the page (pageSettings) of its mentions' keys and its places, collected
// once. Nothing is read for a page with neither. A read that fails keeps the
// page whole, logged once.
func toldAndHeard(ctx context.Context, reader string, items []dgraphModels.UnifiedActivityItem) []dgraphModels.UnifiedActivityItem {
	keyOf := make([]string, len(items))
	var keys, channelIDs, chats []string
	for i := range items {
		if keyOf[i] = toldMentionKey(items[i], reader); keyOf[i] != "" {
			keys = append(keys, keyOf[i])
		}
		switch kind, id := activityScope(&items[i]); kind {
		case "channel":
			channelIDs = append(channelIDs, id)
		case "chat":
			chats = append(chats, id)
		}
	}
	if len(keys) == 0 && len(channelIDs) == 0 && len(chats) == 0 {
		return items
	}
	s, err := pageSettings(ctx, reader, keys, channelIDs, chats)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Activity: %s's notices and notification settings couldn't be read, keeping the page: %+v", reader, err)
		return items
	}
	return keepHeard(dropToldMentions(items, keyOf, s.Held), s.ByChannel, s.ByChat)
}
