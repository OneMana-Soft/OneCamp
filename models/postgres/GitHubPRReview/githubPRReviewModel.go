package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type GitHubPRReview struct {
	Id              uuid.UUID `json:"id"`
	TaskId          uuid.UUID `json:"task_id"`
	GitHubLogin     string    `json:"github_login"`
	GitHubAvatarURL string    `json:"github_avatar_url"`
	GitHubHTMLURL   string    `json:"github_html_url"`
	ReviewState     string    `json:"review_state"`
	SubmittedAt     time.Time `json:"submitted_at"`
}

const GITHUB_PR_REVIEW_COLS = `id, task_id, github_login, github_avatar_url, github_html_url, review_state, submitted_at`

func CreateGitHubPRReview(query string, id uuid.UUID, taskId uuid.UUID, githubLogin string, githubAvatarURL string, githubHTMLURL string, reviewState string, submittedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id, taskId, githubLogin, githubAvatarURL, githubHTMLURL, reviewState, submittedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateGitHubPRReview Failed err: %+v", err)
		return err
	}
	return nil
}

func UpsertGitHubPRReview(query string, taskId uuid.UUID, githubLogin string, githubAvatarURL string, githubHTMLURL string, reviewState string, submittedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, taskId, githubLogin, githubAvatarURL, githubHTMLURL, reviewState, submittedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpsertGitHubPRReview Failed err: %+v", err)
		return err
	}
	return nil
}

func GetGitHubPRReviewsByTaskId(query string, taskId uuid.UUID) ([]*GitHubPRReview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, taskId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubPRReviewsByTaskId Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var reviews []*GitHubPRReview
	for rows.Next() {
		var review GitHubPRReview
		var avatarURL, htmlURL sql.NullString

		if err := rows.Scan(&review.Id, &review.TaskId, &review.GitHubLogin, &avatarURL, &htmlURL, &review.ReviewState, &review.SubmittedAt); err != nil {
			helpers.LogErrorWithContext(ctx, "models/GetGitHubPRReviewsByTaskId scan Failed err: %+v", err)
			return nil, err
		}
		if avatarURL.Valid {
			review.GitHubAvatarURL = avatarURL.String
		}
		if htmlURL.Valid {
			review.GitHubHTMLURL = htmlURL.String
		}
		reviews = append(reviews, &review)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubPRReviewsByTaskId rows iteration Failed err: %+v", err)
		return nil, err
	}

	return reviews, nil
}

func DeleteGitHubPRReviewsByTaskId(query string, taskId uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, taskId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteGitHubPRReviewsByTaskId Failed err: %+v", err)
		return err
	}
	return nil
}

// ExecRawContext executes a raw SQL query with the provided arguments.
// Used by the domain layer for batched operations.
func ExecRawContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	return postgresInit.DBConn.SqlDB.ExecContext(ctx2, query, args...)
}
