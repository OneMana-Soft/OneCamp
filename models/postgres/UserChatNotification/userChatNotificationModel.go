package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

func CreateChatNotificationType(query string, userID string, toUUID string, grpId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		toUUID,
		grpId,
		notificationType,
		updatedAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateCharNotificationType Failed to create/update user fcm token err: %+v",
			err)
		return
	}

	return
}

func UpdateChatNotificationType(query string, userID string, toUserId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		notificationType,
		updatedAt,
		userID,
		toUserId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatNotification Failed to update user's chat notification err: %+v",
			err)
		return
	}

	return
}

func GetNotificationTypeByUserIdAndToUserId(query string, userId string, channelId string) (notificationType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userId, channelId).Scan(&notificationType)

	if err != nil {
		if err != sql.ErrNoRows {
			helpers.LogErrorWithContext(ctx,
				"models/GetNotificationTypeByUserIdAndToUserId Failed to user's notification err: %+v",
				err)
			return
		}
		err = nil
	}

	return
}

func BulkCreateChatNotification(query string, values ...interface{}) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkCreateChatNotification Failed to execute bulk insert err: %+v",
			err)
		return err
	}

	return nil
}

func GetEligibleUsersForChatActivity(query string, grpId string, userId string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, grpId, userId, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetEligibleUsersForChatActivity Failed to get user's notification err: %+v",
			err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetEligibleUsersForChatActivity Failed to scan user_id err: %+v",
				err)
			return nil, err
		}
		userIds = append(userIds, uid)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserChatNotification rows iteration failed err: %+v", err)
		return
	}

	return
}
