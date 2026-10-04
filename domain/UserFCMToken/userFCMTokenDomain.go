package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserFCMToken"
)

func CreateOrUpdateUserFCMToken(ctx context.Context, userID string, deviceId string, fcmToken string, updatedAt time.Time) (err error) {
	query := `
		INSERT INTO users_fcm_token (user_id, device_id, fcm_token, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, device_id, fcm_token)
		DO UPDATE SET fcm_token = EXCLUDED.fcm_token,
		updated_at = EXCLUDED.updated_at;
	`
	err = models.CreateOrUpdateUserFCMToken(query, userID, deviceId, fcmToken, updatedAt)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateUserFCMToken Failed to create or update user's fcm token err: %+v",
			err)
		return
	}
	return
}

func DeleteByUserId(ctx context.Context, userId string) (err error) {
	query := `
		DELETE FROM users_fcm_token
        WHERE user_id = $1
	`

	err = models.DeleteByUserId(query, userId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteByUserId Failed to remove user's fcm token err: %+v",
			err)
		return
	}
	return
}

func DeleteByUserIdAndDeviceId(ctx context.Context, userId string, deviceId string) (err error) {
	query := `
		DELETE FROM users_fcm_token
        WHERE user_id = $1
		AND device_id = $2;
	`

	err = models.DeleteByUserIdAndDeviceId(query, userId, deviceId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteByUserIdAndDeviceId Failed to remove user's fcm token err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokenByUserId(ctx context.Context, userId string) (tokens []string, err error) {
	query := `
		SELECT fcm_token 
		FROM users_fcm_token 
		WHERE user_id = $1 AND` + notPaused("users_fcm_token.user_id") + `
	`

	tokens, err = models.GetFCMTokenByUserId(query, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokenByUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokenByListOfUserId(ctx context.Context, userIds []string) (tokens []string, err error) {
	if len(userIds) == 0 {
		return []string{}, nil
	}

	query := `
		SELECT fcm_token
		FROM users_fcm_token
		WHERE user_id IN (` + helpers.Placeholders(len(userIds)) + `) AND` + notPaused("users_fcm_token.user_id")

	tokens, err = models.GetFCMTokenByListOfUserId(query, userIds)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokenByListOfUserId Failed to get user's and their fcm token's err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewChannelActivityExcludingUserId(ctx context.Context, userId string, channelId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	query := `
		WITH filtered_users AS (
			SELECT ucn.user_id, fcm.fcm_token
			FROM users_fcm_token fcm
			JOIN users_channel_notification ucn 
			  ON fcm.user_id = ucn.user_id
			WHERE ucn.channel_id = $1
			  AND ucn.user_id != $2
			  AND` + notPaused("fcm.user_id") + `
			  AND (
				  (ucn.notification_type = 'all' AND ucn.user_id = ANY($3)) OR
				  (ucn.notification_type = 'mention' AND ucn.user_id = ANY($4))
			  )
		)
		SELECT fcm_token FROM filtered_users;
    `
	tokens, err = models.GetFCMTokensForNewChannelActivity(query, channelId, userId, membersList, mentionsList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokensForNewChannelActivity Failed to get user's fcm token err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewProjectActivityExcludingUserId(ctx context.Context, userId string, projectId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	query := `
		SELECT 
			uft.fcm_token
		FROM 
			users_project_notification upn
		JOIN 
			users_fcm_token uft
		ON 
			upn.user_id = uft.user_id
		WHERE 
			upn.project_id = $1
			AND upn.user_id != $2
			AND` + notPaused("uft.user_id") + `
			AND (
					(upn.notification_type = 'all' AND upn.user_id = ANY($3)) OR
					(upn.notification_type = 'mention' AND upn.user_id = ANY($4))
				);
	`

	tokens, err = models.GetFCMTokensForNewProjectActivityExcludingUserId(query, projectId, userId, membersList, mentionsList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokensForNewProjectActivityExcludingUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewChatActivityExcludingUserId(ctx context.Context, userId string, grpId string, mentionsList []string) (tokens []string, err error) {
	query := `
		SELECT 
			uft.fcm_token
		FROM 
			users_chat_notification ucn
		JOIN 
			users_fcm_token uft
		ON 
			ucn.user_id = uft.user_id
		WHERE 
			ucn.grp_id = $1
			AND ucn.user_id != $2
			AND` + notPaused("uft.user_id") + `
			AND (
					(ucn.notification_type = 'all') OR
					(ucn.notification_type = 'mention' AND ucn.user_id = ANY($3))
				);
	`

	tokens, err = models.GetFCMTokensForNewChatActivity(query, grpId, userId, mentionsList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokensForNewChatActivity Failed to get user's fcm token err: %+v",
			err)
		return
	}
	return
}

func DeleteFromListOfFCMToken(ctx context.Context, tokens []string) (err error) {
	query := `
		DELETE FROM users_fcm_token 
		WHERE fcm_token = ANY($1)
	`

	err = models.DeleteFromListOfFCMToken(query, tokens)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteFromListOfFCMToken Failed to delete user's fcm token err: %+v",
			err)
		return
	}
	return
}
