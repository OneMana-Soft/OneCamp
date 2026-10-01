package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	prReviewModel "github.com/akashc777/OneCamp/models/postgres/GitHubPRReview"
	"github.com/google/uuid"
)

// UpsertGitHubPRReview inserts or updates a single PR review record for a task.
func UpsertGitHubPRReview(ctx context.Context, taskId uuid.UUID, githubLogin string, githubAvatarURL string, githubHTMLURL string, reviewState string, submittedAt time.Time) error {
	query := `
		INSERT INTO github_pr_reviews (task_id, github_login, github_avatar_url, github_html_url, review_state, submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (task_id, github_login) DO UPDATE SET
			review_state = EXCLUDED.review_state,
			submitted_at = EXCLUDED.submitted_at,
			github_avatar_url = EXCLUDED.github_avatar_url,
			github_html_url = EXCLUDED.github_html_url
	`
	err := prReviewModel.UpsertGitHubPRReview(query, taskId, githubLogin, githubAvatarURL, githubHTMLURL, reviewState, submittedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpsertGitHubPRReview Failed err: %+v", err)
		return err
	}
	return nil
}

// BatchUpsertGitHubPRReview inserts or updates multiple PR review records in one query.
func BatchUpsertGitHubPRReview(ctx context.Context, taskId uuid.UUID, reviews []prReviewModel.GitHubPRReview) error {
	if len(reviews) == 0 {
		return nil
	}
	// Build multi-row VALUES clause.
	placeholders := make([]string, 0, len(reviews))
	args := make([]interface{}, 0, len(reviews)*6)
	for i, r := range reviews {
		off := i * 6
		placeholders = append(placeholders, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d)", off+1, off+2, off+3, off+4, off+5, off+6))
		args = append(args, taskId, r.GitHubLogin, r.GitHubAvatarURL, r.GitHubHTMLURL, r.ReviewState, r.SubmittedAt)
	}
	query := fmt.Sprintf(`
		INSERT INTO github_pr_reviews (task_id, github_login, github_avatar_url, github_html_url, review_state, submitted_at)
		VALUES %s
		ON CONFLICT (task_id, github_login) DO UPDATE SET
			review_state = EXCLUDED.review_state,
			submitted_at = EXCLUDED.submitted_at,
			github_avatar_url = EXCLUDED.github_avatar_url,
			github_html_url = EXCLUDED.github_html_url
	`, strings.Join(placeholders, ","))

	_, err := prReviewModel.ExecRawContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BatchUpsertGitHubPRReview Failed err: %+v", err)
		return err
	}
	return nil
}

// GetGitHubPRReviewsByTaskId returns all PR reviews for a task, ordered by submitted_at DESC.
func GetGitHubPRReviewsByTaskId(ctx context.Context, taskId uuid.UUID) ([]*prReviewModel.GitHubPRReview, error) {
	query := `
		SELECT ` + prReviewModel.GITHUB_PR_REVIEW_COLS + `
		FROM github_pr_reviews
		WHERE task_id = $1
		ORDER BY submitted_at DESC
	`
	reviews, err := prReviewModel.GetGitHubPRReviewsByTaskId(query, taskId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubPRReviewsByTaskId Failed err: %+v", err)
		return nil, err
	}
	return reviews, nil
}
