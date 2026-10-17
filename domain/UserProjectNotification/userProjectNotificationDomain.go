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

// taskCommentAudienceQuery is who of a project's ($1) people, other than a
// comment's writer ($2), hears of a comment on one of its tasks by their
// setting for the project: everything, if they are in the task's thread ($3)
// or the comment is for them ($4: whom it mentions, whom the task is given
// to); mentions only, if it is for them; nothing, if they muted the project.
// Someone with no setting for the project hears nothing.
//
// Someone who asked to hear of everything used to be told only of the threads
// they had commented in, so less than someone who asked for mentions only: a
// mention, or a comment on a task given to them, never reached them.
const taskCommentAudienceQuery = `
	SELECT user_id
	FROM users_project_notification
	WHERE project_id = $1
		AND user_id != $2
		AND (
			(notification_type = 'all' AND (user_id = ANY($3) OR user_id = ANY($4))) OR
			(notification_type = 'mention' AND user_id = ANY($4))
		)
`

// GetTaskCommentAudience is who of a project's people hears of a comment on
// one of its tasks by their setting for the project (taskCommentAudienceQuery).
// A setting is no proof someone is still in the project: whoever tells them
// asks who may read the task too.
func GetTaskCommentAudience(ctx context.Context, projectID string, writerID string, thread []string, forThem []string) (userIDs []string, err error) {
	userIDs, err = models.GetTaskCommentAudience(taskCommentAudienceQuery, projectID, writerID, thread, forThem)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetTaskCommentAudience Failed to read who hears of a comment on a task of project %s err: %+v",
			projectID, err)
	}
	return
}
