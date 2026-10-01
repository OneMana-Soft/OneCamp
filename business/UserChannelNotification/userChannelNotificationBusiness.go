package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
)

func CreateChannelNotificationType(userId string, channelId string, notificationType string) {
	ctx := context.Background()
	currentTime := time.Now()
	err := domain.CreateChannelNotificationType(ctx, userId, channelId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChannelNotificationType Failed to create channel notification err: %+v",
			err)
		return
	}
}

func UpdateChannelNotificationType(userId string, channelId string, notificationType string) (err error) {
	ctx := context.Background()

	currentTime := time.Now()
	err = domain.UpdateChannelNotificationType(ctx, userId, channelId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelNotificationType Failed to update channel notification err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormChannel(userId string, channelId string) {
	ctx := context.Background()

	err := domain.DeleteNotificationTypeWhenUserIsRemovedFormChannel(ctx, userId, channelId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelNotificationType Failed to delete user's channel notification err: %+v",
			err)
		return
	}

}

func GetNotificationTypeByUserIdAndChannelId(userId string, channelId string) (notificationType string, err error) {
	ctx := context.Background()

	notificationType, err = domain.GetNotificationTypeByUserIdAndChannelId(ctx, userId, channelId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelNotificationType Failed to delete user's channel notification err: %+v",
			err)
		return
	}
	return
}

func GetEligibleUsersForChannelActivity(ctx context.Context, channelId string, excludeUserID string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	userIds, err = domain.GetEligibleUsersForChannelActivity(ctx, channelId, excludeUserID, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetEligibleUsersForChannelActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
