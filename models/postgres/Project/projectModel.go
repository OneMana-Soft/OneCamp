package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"

	"github.com/google/uuid"
)

type Project struct {
	Id          uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	ProjectName string    `gorm:"unique" json:"project_name"`
	TeamId      uuid.UUID
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   time.Time
}

func CreateProject(query string, projectUUID uuid.UUID, projectName string, teamUUID uuid.UUID, createdByUUID uuid.UUID, createdTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		projectUUID,
		projectName,
		teamUUID,
		createdByUUID,
		createdTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateProject Failed to create new project err: %+v",
			err)
		return
	}

	return
}

func CheckIfProjectExistByProjectNameAndTeamUUID(query string, projectName string, teamId uuid.UUID) (exist bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, projectName, teamId).Scan(&exist)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CheckIfProjectExistByProjectNameAndTeamUUID Failed to check if project name exist in given team ID err: %+v",
			err)
		return
	}

	return
}

func GetProjectByUUID(query string, uuid uuid.UUID) (project *Project, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var projectInfo Project
	var projectName sql.NullString
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uuid)
	err = row.Scan(
		&projectInfo.Id,
		&projectName,
		&projectInfo.CreatedBy,
		&projectInfo.TeamId,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetProjectByUUID Failed to get project err: %+v",
			err)
		return
	}
	if projectName.Valid {
		projectInfo.ProjectName = projectName.String
	}

	if createdAt.Valid {
		projectInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		projectInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		projectInfo.DeletedAt = deletedAt.Time
	}

	return &projectInfo, nil

}

func UpdateProjectNameByProjectUUID(query string, projectName string, currentTime time.Time, projectUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		projectName,
		currentTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectNameByProjectUUID Failed to update project name err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectDeletedTimeByUUID(query string, projectUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deleteTime,
		updateTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectDeletedTimeByUUID Failed to update project's delete time err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectDeletedTimeToNullByUUID(query string, projectUUID uuid.UUID, updateTime *time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		updateTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectDeletedTimeToNullByUUID Failed to update project's delete time to null err: %+v",
			err)
		return
	}

	return
}

// HardDeleteProject removes a project row outright. Compensation only.
//
// Everything else soft-deletes, and should. This exists for one case: the row was inserted here
// and the matching Dgraph node could not be created, so it describes a project no part of the
// product can see — project listings read Dgraph. A soft delete would not help, because
// projects.project_name carries a plain UNIQUE that a soft-deleted row still occupies, so the
// name would stay reserved by something invisible and the user could never retry with it.
//
// Safe in that window and only in that window: projects(id) is referenced by tasks,
// users_project_notification and github_links with no ON DELETE, so this fails rather than
// cascades once the project has any of them. The compensation path runs before they are written.
func HardDeleteProject(query string, projectUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, projectUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteProject Failed to delete project row err: %+v", err)
		return
	}
	return
}
