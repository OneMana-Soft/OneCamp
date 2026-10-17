package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

// activitiesPerStatement bounds one insert of CreateGitHubTaskActivities.
const activitiesPerStatement = 1000

// CreateGitHubTaskActivities inserts the activities, a statement per
// thousand, each with its own id and time; a payload of none is {}.
func CreateGitHubTaskActivities(ctx context.Context, activities []GitHubTaskActivity) error {
	for start := 0; start < len(activities); start += activitiesPerStatement {
		chunk := activities[start:min(start+activitiesPerStatement, len(activities))]
		ids, tasks := make([]uuid.UUID, len(chunk)), make([]uuid.UUID, len(chunk))
		kinds, at := make([]string, len(chunk)), make([]string, len(chunk))
		logins, avatars, addresses := make([]sql.NullString, len(chunk)), make([]sql.NullString, len(chunk)), make([]sql.NullString, len(chunk))
		titles, bodies, payloads := make([]sql.NullString, len(chunk)), make([]sql.NullString, len(chunk)), make([]sql.NullString, len(chunk))
		text := func(s *string) sql.NullString {
			if s == nil {
				return sql.NullString{}
			}
			return sql.NullString{String: *s, Valid: true}
		}
		for i, a := range chunk {
			ids[i], tasks[i], kinds[i], at[i] = a.ID, a.TaskID, a.ActivityType, a.CreatedAt.UTC().Format(time.RFC3339Nano)
			logins[i], avatars[i], addresses[i] = text(a.GitHubLogin), text(a.GitHubAvatarURL), text(a.GitHubHTMLURL)
			titles[i], bodies[i], payloads[i] = text(a.Title), text(a.Body), text(a.Payload)
		}
		dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
		_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
			INSERT INTO github_task_activities (id, task_id, activity_type, github_login, github_avatar_url, github_html_url, title, body, payload, created_at)
			SELECT id, task_id, activity_type, github_login, github_avatar_url, github_html_url, title, body, COALESCE(payload::jsonb, '{}'::jsonb), created_at
			FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::timestamptz[])
				AS u(id, task_id, activity_type, github_login, github_avatar_url, github_html_url, title, body, payload, created_at)`,
			pq.Array(ids), pq.Array(tasks), pq.Array(kinds), pq.Array(logins), pq.Array(avatars), pq.Array(addresses),
			pq.Array(titles), pq.Array(bodies), pq.Array(payloads), pq.Array(at))
		cancel()
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/CreateGitHubTaskActivities Failed err: %+v", err)
			return err
		}
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
