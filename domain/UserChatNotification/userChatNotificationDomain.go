package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserChatNotification"
)

func CreateChatNotificationType(ctx context.Context, userId string, toUUID string, grpID string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		INSERT INTO users_chat_notification 
		(user_id, grp_id, notification_type, updated_at)
		VALUES ($1, $3, $4, $5),
		($2, $3, $4, $5)
	`

	err = models.CreateChatNotificationType(query, userId, toUUID, grpID, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateCharNotificationType Failed to create user's chat notification type err: %+v",
			err)
		return
	}
	return
}

func BulkCreateChatNotification(ctx context.Context, userIDs []string, grpID string, notificationType string, lastSeenChannelTime time.Time) (err error) {

	if len(userIDs) == 0 {
		return nil
	}

	query := `INSERT INTO users_chat_notification (user_id, grp_id, notification_type, updated_at) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	values = append(values, grpID, notificationType, lastSeenChannelTime)

	for i := 0; i < len(userIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $1, $2, $3)", i+4))
		values = append(values, userIDs[i])
	}

	query += strings.Join(placeholders, ",")

	err = models.BulkCreateChatNotification(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateChatNotification Failed to create chat notification err: %+v",
			err)
		return err
	}

	return nil
}

func BulkCreateDMNotificationsIfNotExists(ctx context.Context, senderId string, toUUIDs []string, grpIDs []string, notificationType string, updateTime time.Time) (err error) {
	// Nothing to do is not the same thing as being handed inconsistent input.
	//
	// This used to return nil for both, which meant a caller whose two lists had drifted
	// apart got no notifications for the ENTIRE batch, with no error and nothing logged.
	// Row i pairs recipient i with grouping id i, so a mismatch is a bug upstream and has to
	// say so; an empty list is genuinely a no-op.
	if len(toUUIDs) == 0 {
		return nil
	}
	if err := helpers.RequireSameLength("domain/BulkCreateDMNotificationsIfNotExists",
		helpers.NamedLen{Name: "toUUIDs", Len: len(toUUIDs)},
		helpers.NamedLen{Name: "grpIDs", Len: len(grpIDs)},
	); err != nil {
		return err
	}

	query := `INSERT INTO users_chat_notification (user_id, grp_id, notification_type, updated_at) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	values = append(values, senderId, notificationType, updateTime)

	// Each pair requires inserting 2 rows (one for sender, one for recipient)
	for i := 0; i < len(toUUIDs); i++ {
		// Sender row: ($1, grpId, type, time)
		placeholders = append(placeholders, fmt.Sprintf("($1, $%d, $2, $3)", len(values)+1))
		values = append(values, grpIDs[i])

		// Recipient row: (toUUId, grpId, type, time)
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $2, $3)", len(values)+1, len(values)+2))
		values = append(values, toUUIDs[i], grpIDs[i])
	}

	query += strings.Join(placeholders, ",")
	query += ` ON CONFLICT (user_id, grp_id) DO NOTHING`

	err = models.BulkCreateChatNotification(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateDMNotificationsIfNotExists Failed to create DM notifications err: %+v",
			err)
		return err
	}
	return nil
}

func UpdateChatNotificationType(ctx context.Context, userId string, grpID string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		UPDATE users_chat_notification
        SET notification_type = $1, updated_at = $2
		WHERE user_id = $3 AND grp_id = $4
	`

	err = models.UpdateChatNotificationType(query, userId, grpID, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChatNotificationType Failed to update user's chat notification type err: %+v",
			err)
		return
	}
	return
}

func GetNotificationTypeByUserIdAndToUserId(ctx context.Context, userId string, grpId string) (notificationType string, err error) {
	query := `
		SELECT notification_type
		FROM users_chat_notification
		WHERE user_id = $1 AND grp_id = $2
	`

	notificationType, err = models.GetNotificationTypeByUserIdAndToUserId(query, userId, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetNotificationTypeByUserIdAndToUserId Failed to user's notification err: %+v",
			err)
		return
	}
	return

}

func GetEligibleUsersForChatActivity(ctx context.Context, grpId string, userId string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	query := `
		SELECT 
			user_id
		FROM 
			users_chat_notification
		WHERE 
			grp_id = $1
			AND user_id != $2
			AND (
					(notification_type = 'all' AND ($4 = false OR user_id = ANY($3))) OR
				  	(notification_type = 'mention' AND user_id = ANY($3))
				);
	`

	userIds, err = models.GetEligibleUsersForChatActivity(query, grpId, userId, mentionsList, limitToMentions)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetEligibleUsersForChatActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
