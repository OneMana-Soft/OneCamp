package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// GetChannelNotificationTypesForUser is channel id → the person's
// notification level, for every channel they have one for.
func GetChannelNotificationTypesForUser(query string, userId string) (levels map[string]string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetChannelNotificationTypesForUser Failed to read notification levels err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	levels = map[string]string{}
	for rows.Next() {
		var channelID, level string
		if err := rows.Scan(&channelID, &level); err != nil {
			return nil, err
		}
		levels[channelID] = level
	}
	return levels, rows.Err()
}
