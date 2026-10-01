package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/GitHubTaskActivity"
	"github.com/google/uuid"
)

func CreateGitHubTaskActivity(ctx context.Context, taskID uuid.UUID, activityType string, githubLogin *string, githubAvatarURL *string, githubHTMLURL *string, title *string, body *string, payload *string) error {
	query := `
		INSERT INTO github_task_activities (id, task_id, activity_type, github_login, github_avatar_url, github_html_url, title, body, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, '{}'::jsonb), $10)
	`
	err := models.CreateGitHubTaskActivity(query, uuid.New(), taskID, activityType, githubLogin, githubAvatarURL, githubHTMLURL, title, body, payload, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateGitHubTaskActivity Failed err: %+v", err)
		return err
	}
	return nil
}

func GetGitHubTaskActivitiesByTaskID(ctx context.Context, taskID uuid.UUID) ([]*models.GitHubTaskActivity, error) {
	query := `
		SELECT id, task_id, activity_type, github_login, github_avatar_url, github_html_url, title, body, payload, created_at
		FROM github_task_activities
		WHERE task_id = $1
		ORDER BY created_at DESC
		LIMIT 200
	`
	activities, err := models.GetGitHubTaskActivitiesByTaskID(query, taskID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubTaskActivitiesByTaskID Failed err: %+v", err)
		return nil, err
	}
	return activities, nil
}
