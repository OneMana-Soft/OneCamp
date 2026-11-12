package domain

import (
	"context"
	"encoding/json"
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

// CreateGitHubTaskActivities records many activities at once, as a push's
// commits are: a statement per thousand, where each had a goroutine and an
// insert of its own. One without an id or a time is given one now.
func CreateGitHubTaskActivities(ctx context.Context, activities []models.GitHubTaskActivity) error {
	for i := range activities {
		if activities[i].ID == uuid.Nil {
			activities[i].ID = uuid.New()
		}
		if activities[i].CreatedAt.IsZero() {
			activities[i].CreatedAt = time.Now()
		}
	}
	if err := models.CreateGitHubTaskActivities(ctx, activities); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateGitHubTaskActivities Failed err: %+v", err)
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

// madeByPR is the key of a pr_opened line's payload on a task a pull request
// made; its value is the pull request's address.
const madeByPR = "made_by_pr"

// MadeByPR is the payload of the pr_opened line of a task the pull request at
// prURL made (the webhook's task for a pull request that names none, and the
// importer's): the task is the pull request's own, so it follows the pull
// request's edits (PRMadeTask).
func MadeByPR(prURL string) *string {
	raw, _ := json.Marshal(map[string]string{madeByPR: prURL})
	payload := string(raw)
	return &payload
}

// PRMadeTask is whether the pull request at prURL made the task (MadeByPR). A
// task a pull request only names, by a key, an address or its branch, is a
// person's, and a pull request's edits never rewrite it.
func PRMadeTask(ctx context.Context, taskID uuid.UUID, prURL string) (bool, error) {
	query := `SELECT EXISTS (SELECT 1 FROM github_task_activities
		WHERE task_id = $1 AND activity_type = 'pr_opened' AND payload ->> $2 = $3)`
	made, err := models.AnyGitHubTaskActivity(ctx, query, taskID, madeByPR, prURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/PRMadeTask Failed err: %+v", err)
		return false, err
	}
	return made, nil
}
