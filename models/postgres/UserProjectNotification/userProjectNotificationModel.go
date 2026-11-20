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
