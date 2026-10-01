package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
)

func CreateOrUpdateUserFCMToken(ctx context.Context, userID string, deviceId string, fcmToken string) (err error) {
	currentTime := time.Now()
	err = domain.CreateOrUpdateUserFCMToken(ctx, userID, deviceId, fcmToken, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateUserFCMToken Failed to create or update user's fcm token err: %+v",
			err)
		return
	}

	return
}

func DeleteByUserId(userId string) {
	ctx := context.Background()
	err := domain.DeleteByUserId(ctx, userId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteByUserId Failed to delete fcm token by userId and deviceId err: %+v",
			err)
	}
}

func DeleteByUserIdAndDeviceId(ctx context.Context, userId string, deviceId string) (err error) {
	err = domain.DeleteByUserIdAndDeviceId(ctx, userId, deviceId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteByUserIdAndDeviceId Failed to delete fcm token by userId and deviceId err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokenByUserId(ctx context.Context, userId string) (tokens []string, err error) {
	tokens, err = domain.GetFCMTokenByUserId(ctx, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFCMTokenByUserId Failed to fcm tokens from userId err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokenByListOfUserId(ctx context.Context, userIds []string) (tokens []string, err error) {
	tokens, err = domain.GetFCMTokenByListOfUserId(ctx, userIds)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFCMTokenByListOfUserId Failed to fcm tokens from list of userId err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewChannelActivityExcludingUserId(ctx context.Context, userId string, channelId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	tokens, err = domain.GetFCMTokensForNewChannelActivityExcludingUserId(ctx, userId, channelId, membersList, mentionsList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFCMTokensForNewChannelActivityExcludingUserId Failed to fcm tokens from list of userId err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewProjectActivityExcludingUserId(ctx context.Context, userId string, projectId string, membersList []string, mentionsList []string) (tokens []string, err error) {
	tokens, err = domain.GetFCMTokensForNewProjectActivityExcludingUserId(ctx, userId, projectId, membersList, mentionsList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFCMTokensForNewProjectActivityExcludingUserId Failed to fcm tokens from list of userId err: %+v",
			err)
		return
	}
	return
}

func GetFCMTokensForNewChatActivityExcludingUserId(ctx context.Context, userId string, grpId string, mentionsList []string) (tokens []string, err error) {
	tokens, err = domain.GetFCMTokensForNewChatActivityExcludingUserId(ctx, userId, grpId, mentionsList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFCMTokensForNewChatActivityExcludingUserId Failed to fcm tokens from list of userId err: %+v",
			err)
		return
	}
	return
}

func DeleteFromListOfFCMToken(ctx context.Context, tokens []string) (err error) {
	err = domain.DeleteFromListOfFCMToken(ctx, tokens)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteFromListOfFCMToken Failed to delete fcm tokens err: %+v",
			err)
		return
	}
	return
}
