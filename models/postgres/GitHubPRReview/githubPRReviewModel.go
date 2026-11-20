package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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

// GetGitHubPRReviewsByTaskId reads the reviews query selects (its one
// parameter the task, GITHUB_PR_REVIEW_COLS in order) on the caller's ctx.
func GetGitHubPRReviewsByTaskId(ctx context.Context, query string, taskId uuid.UUID) ([]*GitHubPRReview, error) {
	return pgexec.Query(ctx, query, func(row pgexec.Row) (*GitHubPRReview, error) {
		var review GitHubPRReview
		var avatarURL, htmlURL sql.NullString
		if err := row.Scan(&review.Id, &review.TaskId, &review.GitHubLogin, &avatarURL, &htmlURL, &review.ReviewState, &review.SubmittedAt); err != nil {
			return nil, err
		}
		review.GitHubAvatarURL, review.GitHubHTMLURL = avatarURL.String, htmlURL.String
		return &review, nil
	}, taskId)
}

// ExecRawContext executes a raw SQL query with the provided arguments: the
// domain's one write of a review (RecordGitHubPRReview).
func ExecRawContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	return postgresInit.DBConn.SqlDB.ExecContext(ctx2, query, args...)
}
