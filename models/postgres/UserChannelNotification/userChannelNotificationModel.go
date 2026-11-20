package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
)

func CreateChannelNotificationType(query string, userID string, channelId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		channelId,
		notificationType,
		updatedAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChannelNotificationType Failed to create user's channel notification err: %+v",
			err)
		return
	}

	return
}

func UpdateChannelNotificationType(query string, userID string, channelId string, notificationType string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		notificationType,
		updatedAt,
		userID,
		channelId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChannelNotificationType Failed to create user's channel notification err: %+v",
			err)
		return
	}

	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormChannel(query string, userID string, channelId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		channelId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteNotificationTypeWhenUserIsRemovedFormChannel Failed to delete user's channel notification err: %+v",
			err)
		return
	}

	return
}

func GetNotificationTypeByUserIdAndChannelId(query string, userId string, channelId string) (notificationType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userId, channelId).Scan(&notificationType)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		helpers.LogErrorWithContext(ctx,
			"models/GetNotificationTypeByUserIdAndChannelId Failed to user's notification err: %+v",
			err)
		return
	}

	return
}

// GetLevelsInChannel runs query, which takes the channel's id and a list of
// user ids and answers (user_id, notification_type) rows, into a map by user
// id. Any error, a row's included, answers no map: a partial list would read
// as fewer members.
func GetLevelsInChannel(query string, channelID string, userIDs []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, channelID, userIDs)
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

// GetMembers runs query, which takes a channel's id and a limit and answers
// (user id, level) rows, into the ids in order and each one's level. Any
// error answers neither: a partial list would read as fewer members.
func GetMembers(ctx context.Context, query string, channelID string, limit int) (ids []string, levels map[string]string, err error) {
	type member struct{ id, level string }
	got, err := pgexec.Query(ctx, query, func(r pgexec.Row) (m member, err error) {
		err = r.Scan(&m.id, &m.level)
		return
	}, channelID, limit)
	if err != nil {
		return nil, nil, err
	}
	ids, levels = make([]string, len(got)), make(map[string]string, len(got))
	for i, m := range got {
		ids[i], levels[m.id] = m.id, m.level
	}
	return ids, levels, nil
}
