package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/LastSeenActivity"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenActivity(ctx context.Context, userID uuid.UUID) (err error) {

	err = domain.CreateOrUpdateLastSeenActivity(ctx, userID, time.Now())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateLastSeenActivity Failed to create or update last seen activity err: %+v",
			err)
		return
	}

	return
}

func GetTotalUnreadActivityCount(ctx context.Context, userDgraphId string, userID uuid.UUID) (totalCount uint64, err error) {

	totalCount, err = domain.GetTotalUnreadActivityCount(ctx, userDgraphId, userID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetTotalUnreadActivityCount Failed to get total unread count err: %+v",
			err)
		return
	}

	return
}
