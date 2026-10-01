package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/LastSeenChannel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenChannel(ctx context.Context, userID uuid.UUID, channelID uuid.UUID) (err error) {

	err = domain.CreateOrUpdateLastSeenChannel(ctx, userID, channelID, time.Now())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateLastSeenChannel Failed to create or update last seen channel err: %+v",
			err)
		return
	}

	return
}

func BulkUpdateLastSeenChannelForUser(ctx context.Context, userID uuid.UUID, channelIDs []uuid.UUID, updatedTime time.Time) (err error) {
	err = domain.BulkUpdateLastSeenChannelForUser(ctx, userID, channelIDs, updatedTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkUpdateLastSeenChannelForUser Failed to bulk update last seen channel err: %+v",
			err)
		return
	}

	return
}
