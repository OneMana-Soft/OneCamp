package domain

import (
	"context"
	"database/sql"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/GitHubCommentMapping"
	"github.com/google/uuid"
)

// GetGitHubCommentUUID returns the OneCamp comment UUID for a GitHub comment ID.
func GetGitHubCommentUUID(ctx context.Context, repoOwner, repoName string, githubCommentID int64) (commentUUID string, err error) {
	query := `SELECT comment_uuid::text FROM github_comment_mappings WHERE github_comment_id = $1 AND repo_owner = $2 AND repo_name = $3`
	commentUUID, err = models.GetGitHubCommentUUID(query, githubCommentID, repoOwner, repoName)
	if err != nil && err != sql.ErrNoRows {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubCommentUUID Failed err: %+v", err)
	}
	return commentUUID, err
}

// UpsertGitHubCommentMapping creates or updates a mapping between a GitHub comment and a OneCamp comment.
func UpsertGitHubCommentMapping(ctx context.Context, repoOwner, repoName string, githubCommentID int64, commentUUID, taskUUID uuid.UUID) error {
	query := `
		INSERT INTO github_comment_mappings (github_comment_id, repo_owner, repo_name, comment_uuid, task_uuid)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (github_comment_id, repo_owner, repo_name)
		DO UPDATE SET comment_uuid = EXCLUDED.comment_uuid, task_uuid = EXCLUDED.task_uuid
	`
	err := models.UpsertGitHubCommentMapping(query, githubCommentID, repoOwner, repoName, commentUUID, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpsertGitHubCommentMapping Failed err: %+v", err)
	}
	return err
}

// GetGitHubCommentIDByCommentUUID returns the GitHub comment ID, repo_owner, repo_name for a OneCamp comment UUID.
func GetGitHubCommentIDByCommentUUID(ctx context.Context, commentUUID uuid.UUID) (githubCommentID int64, repoOwner, repoName string, err error) {
	query := `SELECT github_comment_id, repo_owner, repo_name FROM github_comment_mappings WHERE comment_uuid = $1 LIMIT 1`
	githubCommentID, repoOwner, repoName, err = models.GetGitHubCommentIDByCommentUUID(query, commentUUID)
	if err != nil && err != sql.ErrNoRows {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubCommentIDByCommentUUID Failed err: %+v", err)
	}
	return githubCommentID, repoOwner, repoName, err
}

// GetGitHubCommentMappingsByTask returns all GitHub comment mappings for a task.
func GetGitHubCommentMappingsByTask(ctx context.Context, taskUUID uuid.UUID) (map[int64]uuid.UUID, error) {
	query := `SELECT github_comment_id, comment_uuid FROM github_comment_mappings WHERE task_uuid = $1`
	mappings, err := models.GetGitHubCommentMappingsByTask(query, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubCommentMappingsByTask Failed err: %+v", err)
	}
	return mappings, err
}
