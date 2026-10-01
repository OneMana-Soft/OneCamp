package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type LastSeenChannel struct {
	UserID       uuid.UUID `json:"user_id"`
	ChannelID    uuid.UUID `json:"channel_id"`
	UserLastSeen time.Time
}

func CreateOrUpdateLastSeenChannel(query string, userID uuid.UUID, channelID uuid.UUID, lastSeenChannelTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		channelID,
		lastSeenChannelTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateLastSeenChannel Failed to create/update last seen channel err: %+v",
			err)
		return
	}

	return
}

func BulkCreateOrUpdateLastSeenChannel(query string, values ...interface{}) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkCreateOrUpdateLastSeenChannel Failed to execute bulk insert/update err: %+v",
			err)
		return err
	}

	return nil
}

// GetLastSeenChannel returns the user's last-seen timestamp for a single
// channel and whether a row exists. A missing row (ok=false) means the user
// has never marked the channel seen — callers treat that as "everything is
// unread" and apply their own lookback floor.
func GetLastSeenChannel(ctx context.Context, userID, channelID uuid.UUID) (lastSeen time.Time, ok bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT user_last_seen FROM last_seen_channel WHERE user_id = $1 AND channel_id = $2`,
		userID, channelID,
	).Scan(&lastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, false, nil
		}
		helpers.LogErrorWithContext(cctx,
			"models/GetLastSeenChannel Failed to read last seen err: %+v", err)
		return time.Time{}, false, err
	}
	return lastSeen, true, nil
}

// GetAllLastSeenChannelsForUser returns a map of channel_id → last-seen for
// every channel the user has a row for, in ONE query. Used by activity
// triage to demote items the user has already viewed without N round-trips.
func GetAllLastSeenChannelsForUser(ctx context.Context, userID uuid.UUID) (map[string]time.Time, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT channel_id, user_last_seen FROM last_seen_channel WHERE user_id = $1`, userID)
	if err != nil {
		helpers.LogErrorWithContext(cctx,
			"models/GetAllLastSeenChannelsForUser failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]time.Time)
	for rows.Next() {
		var chID uuid.UUID
		var ts time.Time
		if err := rows.Scan(&chID, &ts); err != nil {
			return nil, err
		}
		out[chID.String()] = ts
	}
	return out, rows.Err()
}
