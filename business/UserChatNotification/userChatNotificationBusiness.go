package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
)

func UpdateChatNotificationType(ctx context.Context, userId string, grpID string, notificationType string) (err error) {

	currentTime := time.Now()
	err = domain.UpdateChatNotificationType(ctx, userId, grpID, notificationType, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChatNotificationType Failed to update user's chat notification type err: %+v",
			err)
		return
	}

	return
}

func CreateChatNotificationType(ctx context.Context, userId string, toUUID string, grpID string, notificationType string, updateTime time.Time) {

	err := domain.CreateChatNotificationType(ctx, userId, toUUID, grpID, notificationType, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChatNotificationType Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func BulkCreateChatNotification(ctx context.Context, userIDs []string, grpID string, notificationType string, lastSeenChannelTime time.Time) {

	err := domain.BulkCreateChatNotification(ctx, userIDs, grpID, notificationType, lastSeenChannelTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateChatNotification Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func BulkCreateDMNotificationsIfNotExists(ctx context.Context, senderId string, toUUIDs []string, grpIDs []string, notificationType string, updateTime time.Time) {

	err := domain.BulkCreateDMNotificationsIfNotExists(ctx, senderId, toUUIDs, grpIDs, notificationType, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/BulkCreateDMNotificationsIfNotExists Failed to create user's chat notification type err: %+v",
			err)
		return
	}

}

func GetNotificationTypeByUserIdAndToUserId(ctx context.Context, userId string, grpId string) (notificationType string, err error) {
	notificationType, err = domain.GetNotificationTypeByUserIdAndToUserId(ctx, userId, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNotificationTypeByUserIdAndToUserId Failed to user's notification err: %+v",
			err)
		return
	}

	return
}

// ForgetChatSetting takes away the person's setting for the DM or group grpID,
// as they leave it.
func ForgetChatSetting(ctx context.Context, userID, grpID string) error {
	return domain.ForgetChatSetting(ctx, userID, grpID)
}

// GetEligibleUsersForChatActivity is who hears of something in the DM or group
// grpId by their setting for it, of those in it now (InConversationNow): a
// setting can outlive someone's place in a group. When who is in it can't be
// read, it is an error and nobody is told.
func GetEligibleUsersForChatActivity(ctx context.Context, grpId string, userId string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	userIds, err = domain.GetEligibleUsersForChatActivity(ctx, grpId, userId, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetEligibleUsersForChatActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	if userIds, err = InConversationNow(ctx, grpId, userIds); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetEligibleUsersForChatActivity couldn't read who is in %s now, so nobody is told err: %+v",
			grpId, err)
		return nil, err
	}
	return
}

// GetLevelsInConversation is each of userIds' setting for the conversation
// grpId, by lower-case user id, for those in it now (InConversationNow). When
// who is in it can't be read, it is an error.
func GetLevelsInConversation(ctx context.Context, grpId string, userIds []string) (map[string]string, error) {
	levels, err := domain.GetLevelsInConversation(ctx, grpId, userIds)
	if err != nil || len(levels) == 0 {
		return levels, err
	}
	ids := make([]string, 0, len(levels))
	for id := range levels {
		ids = append(ids, id)
	}
	in, err := InConversationNow(ctx, grpId, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLevelsInConversation couldn't read who is in %s now err: %+v", grpId, err)
		return nil, err
	}
	kept := make(map[string]string, len(in))
	for _, id := range in {
		kept[id] = levels[id]
	}
	return kept, nil
}
