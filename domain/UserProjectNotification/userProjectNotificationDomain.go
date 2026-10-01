package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserProjectNotification"
)

func CreateProjectNotificationType(ctx context.Context, userId string, projectId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		INSERT INTO users_project_notification 
		(user_id, project_id, notification_type, updated_at)
		VALUES ($1, $2, $3, $4)
	`

	err = models.CreatePojectNotificationType(query, userId, projectId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectNotificationType Failed to create user's project notification type err: %+v",
			err)
		return
	}
	return
}

func UpdateProjectNotificationType(ctx context.Context, userId string, projectId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		UPDATE users_project_notification
        SET notification_type = $1, updated_at = $2
		WHERE user_id = $3 AND project_id = $4
	`

	err = models.UpdateProjectNotificationType(query, userId, projectId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectNotificationType Failed to update user's project notification type err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormProject(ctx context.Context, userId string, projectId string) (err error) {
	query := `
		DELETE FROM users_project_notification
		WHERE user_id = $1 AND project_id = $2
	`

	err = models.DeleteNotificationTypeWhenUserIsRemovedFormProject(query, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteNotificationTypeWhenUserIsRemovedFormProject Failed to delete user's project notification type err: %+v",
			err)
		return
	}
	return
}

func GetNotificationTypeByUserIdAndProjectId(ctx context.Context, userId string, projectId string) (notificationType string, err error) {
	query := `
		SELECT notification_type
		FROM users_project_notification
		WHERE user_id = $1 AND project_id = $2
	`

	notificationType, err = models.GetNotificationTypeByUserIdAndProjectId(query, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetNotificationTypeByUserIdAndProjectId Failed to user's notification err: %+v",
			err)
		return
	}
	return

}

func GetEligibleUsersForProjectActivity(ctx context.Context, projectId string, excludeUserID string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	query := `
		SELECT 
			user_id
		FROM 
			users_project_notification
		WHERE 
			project_id = $1
			AND user_id != $2
			AND (
				  (notification_type = 'all' AND ($4 = false OR user_id = ANY($3))) OR
				  (notification_type = 'mention' AND user_id = ANY($3))
			  )
	`
	userIds, err = models.GetEligibleUsersForProjectActivity(query, projectId, excludeUserID, mentionsList, limitToMentions)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetEligibleUsersForProjectActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}
