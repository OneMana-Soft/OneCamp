package business

// How many were unread as a DM or a group opened (chat B27). The app draws
// the line where someone left off from it. Reading a conversation's latest
// page marks it read, so the count is taken in that request, just before the
// mark: the sidebar's, asked for at the same moment, could come after it and
// say none.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
	"github.com/google/uuid"
)

// unreadOf is who's unread count in a DM or a group, by the read state's one
// rule (models/postgres/ReadState); a seam for tests.
var unreadOf = func(ctx context.Context, who uuid.UUID, grouping string) (int, error) {
	p, err := readStateModels.PositionOf(ctx, readStateModels.Chat, who, grouping)
	return p.Unread, err
}

// unreadAsOpened is who's unread count in the conversation before this read
// marks it read; nil when it can't be read, and the page is answered without
// it (the app then reads the sidebar's).
func unreadAsOpened(ctx context.Context, who uuid.UUID, grouping string) *int {
	n, err := unreadOf(ctx, who, grouping)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat unread as %s opened left out err: %+v", grouping, err)
		return nil
	}
	return &n
}
