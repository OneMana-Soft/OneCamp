package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
)

func CreateProjectNotificationType(userId string, projectId string, notificationType string) {
	ctx := context.Background()
	currentTime := time.Now()
	err := domain.CreateProjectNotificationType(ctx, userId, projectId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateProjectNotificationType Failed to create project notification err: %+v",
			err)
		return
	}
}

func UpdateProjectNotificationType(userId string, projectId string, notificationType string) (err error) {
	ctx := context.Background()

	currentTime := time.Now()
	err = domain.UpdateProjectNotificationType(ctx, userId, projectId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateProjectNotificationType Failed to update project notification err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormProject(userId string, projectId string) {
	ctx := context.Background()

	err := domain.DeleteNotificationTypeWhenUserIsRemovedFormProject(ctx, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteNotificationTypeWhenUserIsRemovedFormProject Failed to delete user's project notification err: %+v",
			err)
		return
	}

}

func GetNotificationTypeByUserIdAndProjectId(userId string, projectId string) (notificationType string, err error) {
	ctx := context.Background()

	notificationType, err = domain.GetNotificationTypeByUserIdAndProjectId(ctx, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNotificationTypeByUserIdAndProjectId Failed to delete user's project notification err: %+v",
			err)
		return
	}
	return
}

func GetEligibleUsersForProjectActivity(ctx context.Context, projectId string, excludeUserID string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	userIds, err = domain.GetEligibleUsersForProjectActivity(ctx, projectId, excludeUserID, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetEligibleUsersForProjectActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
