package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type LastSeenActivity struct {
	UserID       uuid.UUID `json:"user_id"`
	UserLastSeen time.Time `json:"user_last_seen"`
}

func CreateOrUpdateLastSeenActivity(query string, userID uuid.UUID, lastSeenActivityTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		lastSeenActivityTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateLastSeenActivity Failed to create/update last seen activity err: %+v",
			err)
		return
	}

	return
}

func GetLastSeenActivityByUserId(query string, userID uuid.UUID) (lastSeenActivityTime time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(
		ctx,
		query,
		userID,
	).Scan(&lastSeenActivityTime)
	if err != nil {
		// Log error only if it's not "no rows" as it's expected for new users
		if err.Error() != "sql: no rows in result set" {
			helpers.LogErrorWithContext(ctx,
				"models/GetLastSeenActivityByUserId Failed to get last seen activity err: %+v",
				err)
		}
		return
	}

	return
}
