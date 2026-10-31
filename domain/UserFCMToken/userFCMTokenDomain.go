package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/lib/pq"
)

func CreateOrUpdateUserFCMToken(ctx context.Context, userID string, deviceId string, fcmToken string, updatedAt time.Time) (err error) {
	query := `
		INSERT INTO users_fcm_token (user_id, device_id, fcm_token, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, device_id, fcm_token)
		DO UPDATE SET fcm_token = EXCLUDED.fcm_token,
		updated_at = EXCLUDED.updated_at;
	`
	_, err = pgexec.Exec(ctx, query, userID, deviceId, fcmToken, updatedAt)

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

	_, err = pgexec.Exec(ctx, query, userId)

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

	_, err = pgexec.Exec(ctx, query, userId, deviceId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteByUserIdAndDeviceId Failed to remove user's fcm token err: %+v",
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

	_, err = pgexec.Exec(ctx, query, pq.Array(tokens))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteFromListOfFCMToken Failed to delete user's fcm token err: %+v",
			err)
		return
	}
	return
}
