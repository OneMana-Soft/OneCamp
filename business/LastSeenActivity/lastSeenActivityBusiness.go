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

// UnreadNotices counts a person's inbox notices since a time (business/Follow
// sets it from its init; until then there are none to count).
var UnreadNotices func(ctx context.Context, userUUID string, since time.Time) (uint64, error)

func GetTotalUnreadActivityCount(ctx context.Context, userDgraphId string, userID uuid.UUID) (totalCount uint64, err error) {

	totalCount, err = domain.GetTotalUnreadActivityCount(ctx, userDgraphId, userID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetTotalUnreadActivityCount Failed to get total unread count err: %+v",
			err)
		return
	}

	// The inbox's notices count as unread too, since the person last looked.
	if UnreadNotices != nil {
		if seen, serr := domain.GetLastSeenActivityByUserId(ctx, userID); serr == nil {
			if n, nerr := UnreadNotices(ctx, userID.String(), seen); nerr == nil {
				totalCount += n
			}
		}
	}

	return
}
