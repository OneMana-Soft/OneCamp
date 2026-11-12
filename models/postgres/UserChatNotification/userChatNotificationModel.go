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

// GetLevelsInConversation runs query ($1 the conversation, $2 the people) and
// answers each person's setting by the id the query reads back.
func GetLevelsInConversation(query string, grpID string, userIDs []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, grpID, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	levels := make(map[string]string, len(userIDs))
	for rows.Next() {
		var userID, level string
		if err := rows.Scan(&userID, &level); err != nil {
			return nil, err
		}
		levels[userID] = level
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return levels, nil
}
