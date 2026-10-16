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

// tokensOfPersonQuery is the devices of one person ($1) the push gate lets
// through now.
var tokensOfPersonQuery = `
		SELECT fcm_token
		FROM users_fcm_token
		WHERE user_id = $1 AND` + pushGate("users_fcm_token.user_id") + `
	`

func GetFCMTokenByUserId(ctx context.Context, userId string) (tokens []string, err error) {
	tokens, err = models.GetFCMTokenByUserId(tokensOfPersonQuery, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokenByUserId Failed to get user's fcm token err: %+v",
			err)
		return
	}
	return
}

// tokensOfPeopleQuery is the devices of n people ($1 to $n) the push gate
// lets through now.
func tokensOfPeopleQuery(n int) string {
	return `
		SELECT fcm_token
		FROM users_fcm_token
		WHERE user_id IN (` + helpers.Placeholders(n) + `) AND` + pushGate("users_fcm_token.user_id")
}

func GetFCMTokenByListOfUserId(ctx context.Context, userIds []string) (tokens []string, err error) {
	if len(userIds) == 0 {
		return []string{}, nil
	}

	tokens, err = models.GetFCMTokenByListOfUserId(tokensOfPeopleQuery(len(userIds)), userIds)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokenByListOfUserId Failed to get user's and their fcm token's err: %+v",
			err)
		return
	}
	return
}

// projectActivityTokensQuery is the devices of a project's ($1) people other
// than whoever acted ($2) who hear of its activity, everything (of $3) or
// mentions (of $4), and whom the push gate lets through now.
var projectActivityTokensQuery = `
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
			AND` + pushGate("uft.user_id") + `
			AND (
					(upn.notification_type = 'all' AND upn.user_id = ANY($3)) OR
					(upn.notification_type = 'mention' AND upn.user_id = ANY($4))
				);
	`

func GetFCMTokensForNewProjectActivityExcludingUserId(ctx context.Context, userId string, projectId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	tokens, err = models.GetFCMTokensForNewProjectActivityExcludingUserId(projectActivityTokensQuery, projectId, userId, membersList, mentionsList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetFCMTokensForNewProjectActivityExcludingUserId Failed to get user's fcm token err: %+v",
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
