package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenChat(ctx context.Context, grpId string, userId uuid.UUID, updatedTime time.Time) (err error) {

	err = domain.CreateOrUpdateLastSeenChat(ctx, userId, grpId, updatedTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateLastSeenChannel Failed to create or update last seen channel err: %+v",
			err)
		return
	}

	return
}

func BulkUpdateLastSeenChatForSender(ctx context.Context, grpIDs []string, userId string, updatedTime time.Time) (err error) {
	err = domain.BulkUpdateLastSeenChatForSender(ctx, grpIDs, userId, updatedTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkUpdateLastSeenChatForSender Failed to bulk update last seen chat err: %+v",
			err)
		return
	}

	return
}

func BulkCreateOrUpdateLastSeenChat(ctx context.Context, grpId string, userIds []string, updatedTime time.Time) (err error) {

	err = domain.BulkCreateOrUpdateLastSeenChat(ctx, userIds, grpId, updatedTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateOrUpdateLastSeenChat Failed to create or update last seen channel err: %+v",
			err)
		return
	}

	return
}

func BulkCreateLastSeenChatIfNotExists(ctx context.Context, userIds []string, grpId string) (err error) {
	err = domain.BulkCreateLastSeenChatIfNotExists(ctx, userIds, grpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateLastSeenChatIfNotExists Failed to create last seen chart err: %+v",
			err)
		return
	}

	return
}
