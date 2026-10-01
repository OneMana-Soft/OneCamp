package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
)

func UpdateChatNotificationType(ctx context.Context, userId string, grpID string, notificationType string) (err error) {

	currentTime := time.Now()
	err = domain.UpdateChatNotificationType(ctx, userId, grpID, notificationType, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChatNotificationType Failed to update user's chat notification type err: %+v",
			err)
		return
	}

	return
}

func CreateChatNotificationType(ctx context.Context, userId string, toUUID string, grpID string, notificationType string, updateTime time.Time) {

	err := domain.CreateChatNotificationType(ctx, userId, toUUID, grpID, notificationType, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatNotificationType Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func BulkCreateChatNotification(ctx context.Context, userIDs []string, grpID string, notificationType string, lastSeenChannelTime time.Time) {

	err := domain.BulkCreateChatNotification(ctx, userIDs, grpID, notificationType, lastSeenChannelTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateChatNotification Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func BulkCreateDMNotificationsIfNotExists(ctx context.Context, senderId string, toUUIDs []string, grpIDs []string, notificationType string, updateTime time.Time) {

	err := domain.BulkCreateDMNotificationsIfNotExists(ctx, senderId, toUUIDs, grpIDs, notificationType, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateDMNotificationsIfNotExists Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func GetNotificationTypeByUserIdAndToUserId(ctx context.Context, userId string, grpId string) (notificationType string, err error) {
	notificationType, err = domain.GetNotificationTypeByUserIdAndToUserId(ctx, userId, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNotificationTypeByUserIdAndToUserId Failed to user's notification err: %+v",
			err)
		return
	}

	return
}

func GetEligibleUsersForChatActivity(ctx context.Context, grpId string, userId string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	userIds, err = domain.GetEligibleUsersForChatActivity(ctx, grpId, userId, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetEligibleUsersForChatActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
