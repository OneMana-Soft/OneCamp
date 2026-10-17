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
