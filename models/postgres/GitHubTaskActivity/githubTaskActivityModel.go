package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type GitHubTaskActivity struct {
	ID              uuid.UUID `json:"id"`
	TaskID          uuid.UUID `json:"task_id"`
	ActivityType    string    `json:"activity_type"`
	GitHubLogin     *string   `json:"github_login,omitempty"`
	GitHubAvatarURL *string   `json:"github_avatar_url,omitempty"`
	GitHubHTMLURL   *string   `json:"github_html_url,omitempty"`
	Title           *string   `json:"title,omitempty"`
	Body            *string   `json:"body,omitempty"`
	Payload         *string   `json:"payload,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

func CreateGitHubTaskActivity(query string, id uuid.UUID, taskID uuid.UUID, activityType string, githubLogin *string, githubAvatarURL *string, githubHTMLURL *string, title *string, body *string, payload *string, createdAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id, taskID, activityType, githubLogin, githubAvatarURL, githubHTMLURL, title, body, payload, createdAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateGitHubTaskActivity Failed err: %+v", err)
		return err
	}
	return nil
}

func GetGitHubTaskActivitiesByTaskID(query string, taskID uuid.UUID) ([]*GitHubTaskActivity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, taskID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubTaskActivitiesByTaskID Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var activities []*GitHubTaskActivity
	for rows.Next() {
		var a GitHubTaskActivity
		var login, avatar, html, title, body, payload sql.NullString
		err := rows.Scan(&a.ID, &a.TaskID, &a.ActivityType, &login, &avatar, &html, &title, &body, &payload, &a.CreatedAt)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/GetGitHubTaskActivitiesByTaskID Scan err: %+v", err)
			continue
		}
		if login.Valid {
			a.GitHubLogin = &login.String
		}
		if avatar.Valid {
			a.GitHubAvatarURL = &avatar.String
		}
		if html.Valid {
			a.GitHubHTMLURL = &html.String
		}
		if title.Valid {
			a.Title = &title.String
		}
		if body.Valid {
			a.Body = &body.String
		}
		if payload.Valid {
			a.Payload = &payload.String
		}
		activities = append(activities, &a)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubTaskActivitiesByTaskID rows iteration err: %+v", err)
		return nil, err
	}
	return activities, nil
}
