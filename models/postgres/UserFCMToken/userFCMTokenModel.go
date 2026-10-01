package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type UserFcmToken struct {
	UserID    uuid.UUID `json:"user_id,omitempty"`
	DeviceID  string    `json:"device_id,omitempty"`
	FCMToken  string    `json:"fcm_token,omitempty"`
	UpdatedAt time.Time
}

type FCMToken struct {
	FCMToken string `json:"fcm_token,omitempty"`
}

func CreateOrUpdateUserFCMToken(query string, userID string, deviceId string, fcmToken string, updatedAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		deviceId,
		fcmToken,
		updatedAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateUserFCMToken Failed to create/update user fcm token err: %+v",
			err)
		return
	}

	return
}

func DeleteByUserId(query string, userId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userId,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteByUserId Failed to delete users's fcm token err: %+v",
			err)
		return
	}

	return
}

func DeleteByUserIdAndDeviceId(query string, userId string, deviceId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userId,
		deviceId,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteByUserIdAndDeviceId Failed to delete users's fcm token err: %+v",
			err)
		return
	}

	return
}

func GetFCMTokenByUserId(query string, userId string) (tokens []string, err error) {

	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokenByUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	// A scan error below returns early, so without this the pooled
	// connection is never released. database/sql only auto-closes when
	// Next() runs to completion.
	defer rows.Close()

	// Process the rows to get the result
	for rows.Next() {
		var userFCMToken FCMToken
		if err = rows.Scan(&userFCMToken.FCMToken); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetFCMTokenByUserId Failed to scan row err: %+v",
				err)
			return
		}

		tokens = append(tokens, userFCMToken.FCMToken)
	}
	// Iteration can stop on a mid-query failure (dropped connection,
	// server-side error) rather than on end-of-rows. Without this the
	// function returns a PARTIAL token list with a nil error, which
	// silently drops push notifications for the users left off it.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserFCMToken rows iteration failed err: %+v", err)
		return
	}

	return

}

func GetFCMTokenByListOfUserId(query string, userIDs []string) (tokens []string, err error) {

	args := make([]interface{}, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokenByListOfUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	// A scan error below returns early, so without this the pooled
	// connection is never released. database/sql only auto-closes when
	// Next() runs to completion.
	defer rows.Close()

	// Process the rows to get the result
	for rows.Next() {
		var userFCMToken FCMToken
		if err = rows.Scan(&userFCMToken.FCMToken); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetFCMTokenByListOfUserId Failed to scan row err: %+v",
				err)
			return
		}

		tokens = append(tokens, userFCMToken.FCMToken)
	}
	// Iteration can stop on a mid-query failure (dropped connection,
	// server-side error) rather than on end-of-rows. Without this the
	// function returns a PARTIAL token list with a nil error, which
	// silently drops push notifications for the users left off it.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserFCMToken rows iteration failed err: %+v", err)
		return
	}

	return
}

func GetFCMTokensForNewChannelActivity(query string, channelId string, userId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, channelId, userId, pq.Array(membersList), pq.Array(mentionsList))

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokensForNewChannelActivity Failed to get user's fcm token err: %+v",
			err)
		return
	}
	// A scan error below returns early, so without this the pooled
	// connection is never released. database/sql only auto-closes when
	// Next() runs to completion.
	defer rows.Close()

	for rows.Next() {
		var userFCMToken FCMToken
		if err = rows.Scan(&userFCMToken.FCMToken); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetFCMTokensForNewChannelActivity Failed to scan row err: %+v",
				err)
			return
		}

		tokens = append(tokens, userFCMToken.FCMToken)
	}
	// Iteration can stop on a mid-query failure (dropped connection,
	// server-side error) rather than on end-of-rows. Without this the
	// function returns a PARTIAL token list with a nil error, which
	// silently drops push notifications for the users left off it.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserFCMToken rows iteration failed err: %+v", err)
		return
	}

	return
}

func GetFCMTokensForNewProjectActivityExcludingUserId(query string, projectId string, userId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, projectId, userId, pq.Array(membersList), pq.Array(mentionsList))

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokensForNewProjectActivityExcludingUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	// A scan error below returns early, so without this the pooled
	// connection is never released. database/sql only auto-closes when
	// Next() runs to completion.
	defer rows.Close()

	for rows.Next() {
		var userFCMToken FCMToken
		if err = rows.Scan(&userFCMToken.FCMToken); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetFCMTokensForNewProjectActivityExcludingUserId Failed to scan row err: %+v",
				err)
			return
		}

		tokens = append(tokens, userFCMToken.FCMToken)
	}
	// Iteration can stop on a mid-query failure (dropped connection,
	// server-side error) rather than on end-of-rows. Without this the
	// function returns a PARTIAL token list with a nil error, which
	// silently drops push notifications for the users left off it.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserFCMToken rows iteration failed err: %+v", err)
		return
	}
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokensForNewProjectActivityExcludingUserId rows iteration err: %+v",
			err)
		return
	}

	return
}

func GetFCMTokensForNewChatActivity(query string, grpId string, userId string, membersList []string) (tokens []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, grpId, userId, pq.Array(membersList))

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetFCMTokensForNewChatActivity Failed to get user's fcm token err: %+v",
			err)
		return
	}
	// A scan error below returns early, so without this the pooled
	// connection is never released. database/sql only auto-closes when
	// Next() runs to completion.
	defer rows.Close()

	for rows.Next() {
		var userFCMToken FCMToken
		if err = rows.Scan(&userFCMToken.FCMToken); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetFCMTokensForNewChatActivity Failed to scan row err: %+v",
				err)
			return
		}

		tokens = append(tokens, userFCMToken.FCMToken)
	}
	// Iteration can stop on a mid-query failure (dropped connection,
	// server-side error) rather than on end-of-rows. Without this the
	// function returns a PARTIAL token list with a nil error, which
	// silently drops push notifications for the users left off it.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserFCMToken rows iteration failed err: %+v", err)
		return
	}

	return
}

func DeleteFromListOfFCMToken(query string, tokens []string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		pq.Array(tokens),
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteFromListOfFCMToken Failed to delete users's fcm token err: %+v",
			err)
		return
	}

	return
}
