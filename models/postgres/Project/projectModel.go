package models

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"

	"github.com/google/uuid"
)

type Project struct {
	Id uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	// ProjectName is unique within the project's team (migration 320).
	ProjectName string `json:"project_name"`
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

// ArchivedProject is an archived project's id and when it was archived.
type ArchivedProject struct {
	Id         uuid.UUID
	ArchivedAt time.Time
}

// ArchivedProjectsWithNamePrefix returns the archived projects whose name
// starts with prefix, read literally (% and _ in it match only themselves).
func ArchivedProjectsWithNamePrefix(ctx context.Context, prefix string) ([]ArchivedProject, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(prefix)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT id, deleted_at FROM projects WHERE project_name LIKE $1 ESCAPE '\' AND deleted_at IS NOT NULL`, escaped+"%")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ArchivedProjectsWithNamePrefix err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []ArchivedProject
	for rows.Next() {
		var p ArchivedProject
		if err := rows.Scan(&p.Id, &p.ArchivedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FirstMadeBefore is the earliest made of the live projects ids that were
// made before t, with its name; ok is false when none was.
func FirstMadeBefore(ctx context.Context, ids []uuid.UUID, t time.Time) (id uuid.UUID, name string, ok bool, err error) {
	if len(ids) == 0 || postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return uuid.Nil, "", false, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT id, project_name FROM projects WHERE id = ANY($1) AND deleted_at IS NULL AND created_at < $2
		 ORDER BY created_at, id LIMIT 1`, ids, t)
	if err != nil {
		return uuid.Nil, "", false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&id, &name); err != nil {
			return uuid.Nil, "", false, err
		}
		return id, name, true, rows.Err()
	}
	return uuid.Nil, "", false, rows.Err()
}

// AnyMadeBefore is whether any live project was made before t.
func AnyMadeBefore(ctx context.Context, t time.Time) (bool, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return false, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var found bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT EXISTS (SELECT 1 FROM projects WHERE deleted_at IS NULL AND created_at < $1)`, t).Scan(&found)
	return found, err
}
