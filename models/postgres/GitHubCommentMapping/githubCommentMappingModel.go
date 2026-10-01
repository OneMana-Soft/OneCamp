package models

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

func GetGitHubCommentUUID(query string, githubCommentID int64, repoOwner, repoName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var commentUUID string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, githubCommentID, repoOwner, repoName).Scan(&commentUUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubCommentUUID Failed err: %+v", err)
		return "", err
	}
	return commentUUID, err
}

func UpsertGitHubCommentMapping(query string, githubCommentID int64, repoOwner, repoName string, commentUUID, taskUUID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, githubCommentID, repoOwner, repoName, commentUUID, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpsertGitHubCommentMapping Failed err: %+v", err)
	}
	return err
}

func GetGitHubCommentIDByCommentUUID(query string, commentUUID uuid.UUID) (int64, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var ghID int64
	var repoOwner, repoName string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, commentUUID).Scan(&ghID, &repoOwner, &repoName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubCommentIDByCommentUUID Failed err: %+v", err)
		return 0, "", "", err
	}
	return ghID, repoOwner, repoName, err
}

func GetGitHubCommentMappingsByTask(query string, taskUUID uuid.UUID) (map[int64]uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubCommentMappingsByTask Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	mappings := make(map[int64]uuid.UUID)
	for rows.Next() {
		var ghID int64
		var commentUUID uuid.UUID
		if err := rows.Scan(&ghID, &commentUUID); err != nil {
			helpers.LogErrorWithContext(ctx, "models/GetGitHubCommentMappingsByTask Scan err: %+v", err)
			continue
		}
		mappings[ghID] = commentUUID
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubCommentMappingsByTask rows err: %+v", err)
		return nil, err
	}
	return mappings, nil
}
