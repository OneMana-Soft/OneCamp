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

type Team struct {
	Id        uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	TeamName  string    `gorm:"unique" json:"team_name"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

func CreateTeam(query string, teamUUD uuid.UUID, teamName string, createdBy uuid.UUID, createdAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		teamUUD,
		teamName,
		createdBy,
		createdAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTeam Failed to create new team err: %+v",
			err)
		return
	}

	return
}

func CheckIfTeamExistByTeamName(query string, teamName string) (exist bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, teamName).Scan(&exist)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CheckIfTeamExistByTeamNameAndProjectUUID Failed to check if team name exist err: %+v",
			err)
		return
	}

	return
}

func GetTeamByUUID(query string, uuid uuid.UUID) (team *Team, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var teamInfo Team
	var teamName sql.NullString
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uuid)
	err = row.Scan(
		&teamInfo.Id,
		&teamName,
		&teamInfo.CreatedBy,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetTeamByUUID Failed to get team err: %+v",
			err)
		return
	}
	if teamName.Valid {
		teamInfo.TeamName = teamName.String
	}

	if createdAt.Valid {
		teamInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		teamInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		teamInfo.DeletedAt = deletedAt.Time
	}

	return &teamInfo, nil

}

func UpdateTeamNameByTeamUUID(query string, teamName string, teamUUID uuid.UUID, currentTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		teamName,
		currentTime,
		teamUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTeamNameByEmailID Failed to update team name err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamDeletedTimeByUUID(query string, teamUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deleteTime,
		updateTime,
		teamUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTeamDeletedTimeByUUID Failed to update teams's delete time err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamDeletedTimeToNullByUUID(query string, teamUUID uuid.UUID, updateTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		updateTime,
		teamUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/ UpdateTeamDeletedTimeToNullByUUID Failed to update team's delete time to null err: %+v",
			err)
		return
	}

	return
}

// HardDeleteTeam removes a team row outright. Compensation only.
//
// Everything else soft-deletes, and should. This exists for one case: the row was inserted here
// and the matching Dgraph node could not be created, so it describes a team no part of the
// product can see — team listings read Dgraph. A soft delete would not help, because teams.
// team_name carries a plain UNIQUE that a soft-deleted row still occupies, so the name would
// stay reserved by something invisible and the user could never retry with it.
//
// Safe in that window and only in that window: teams(id) is referenced by projects.team_id with
// no ON DELETE, so this fails rather than cascades once the team has a project. The compensation
// path runs before any project can exist.
func HardDeleteTeam(query string, teamUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteTeam Failed to delete team row err: %+v", err)
		return
	}
	return
}
