package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

func CreatePojectNotificationType(query string, userID string, projectId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		projectId,
		notificationType,
		updatedAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreatePojectNotificationType Failed to create user's project notification err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectNotificationType(query string, userID string, projectId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		notificationType,
		updatedAt,
		userID,
		projectId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectNotificationType Failed to create user's project notification err: %+v",
			err)
		return
	}

	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormProject(query string, userID string, projectId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		projectId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteNotificationTypeWhenUserIsRemovedFormChannel Failed to delete user's channel notification err: %+v",
			err)
		return
	}

	return
}

func GetNotificationTypeByUserIdAndProjectId(query string, userId string, projectId string) (notificationType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userId, projectId).Scan(&notificationType)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetNotificationTypeByUserIdAndProjectId Failed to user's notification err: %+v",
			err)
		return
	}

	return
}

func GetEligibleUsersForProjectActivity(query string, projectId string, excludeUserID string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, projectId, excludeUserID, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetEligibleUsersForProjectActivity Failed to get user's notification err: %+v",
			err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetEligibleUsersForProjectActivity Failed to scan user_id err: %+v",
				err)
			return nil, err
		}
		userIds = append(userIds, uid)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserProjectNotification rows iteration failed err: %+v", err)
		return
	}

	return
}
