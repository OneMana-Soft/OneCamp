package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserChannelNotification"
)

func CreateChannelNotificationType(ctx context.Context, userId string, channelId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		INSERT INTO users_channel_notification 
		(user_id, channel_id, notification_type, updated_at)
		VALUES ($1, $2, $3, $4)
	`

	err = models.CreateChannelNotificationType(query, userId, channelId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChannelNotificationType Failed to create user's channel notification type err: %+v",
			err)
		return
	}
	return
}

func UpdateChannelNotificationType(ctx context.Context, userId string, channelId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		UPDATE users_channel_notification
        SET notification_type = $1, updated_at = $2
		WHERE user_id = $3 AND channel_id = $4
	`

	err = models.UpdateChannelNotificationType(query, userId, channelId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChannelNotificationType Failed to update user's channel notification type err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormChannel(ctx context.Context, userId string, channelId string) (err error) {
	query := `
		DELETE FROM users_channel_notification
		WHERE user_id = $1 AND channel_id = $2
	`

	err = models.DeleteNotificationTypeWhenUserIsRemovedFormChannel(query, userId, channelId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteNotificationTypeWhenUserIsRemovedFormChannel Failed to delete user's channel notification type err: %+v",
			err)
		return
	}
	return
}

func GetNotificationTypeByUserIdAndChannelId(ctx context.Context, userId string, channelId string) (notificationType string, err error) {
	query := `
		SELECT notification_type
		FROM users_channel_notification
		WHERE user_id = $1 AND channel_id = $2
	`

	notificationType, err = models.GetNotificationTypeByUserIdAndChannelId(query, userId, channelId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetNotificationTypeByUserIdAndChannelId Failed to user's notification err: %+v",
			err)
		return
	}
	return

}

func GetEligibleUsersForChannelActivity(ctx context.Context, channelId string, excludeUserID string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	query := `
		SELECT 
			user_id
		FROM 
			users_channel_notification
		WHERE 
			channel_id = $1
			AND user_id != $2
			AND (
				  (notification_type = 'all' AND ($4 = false OR user_id = ANY($3))) OR
				  (notification_type = 'mention' AND user_id = ANY($3))
			  )
	`
	userIds, err = models.GetEligibleUsersForChannelActivity(query, channelId, excludeUserID, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetEligibleUsersForChannelActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
