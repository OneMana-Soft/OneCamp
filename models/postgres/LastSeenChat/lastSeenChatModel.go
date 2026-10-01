package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type LastSeenChat struct {
	UserID       uuid.UUID `json:"user_id"`
	GroupingID   uuid.UUID `json:"grp_id"`
	UserLastSeen time.Time
}

func CreateOrUpdateLastSeenChat(query string, userID uuid.UUID, groupingID string, lastSeenChannelTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userID,
		groupingID,
		lastSeenChannelTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateLastSeenChat Failed to create/update last seen chat err: %+v",
			err)
		return
	}

	return
}

func BulkCreateOrUpdateLastSeenChat(query string, values ...interface{}) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkInsertLastSeenChat Failed to execute bulk insert err: %+v",
			err)
		return err
	}

	return nil
}

// GetLastSeenChat returns the user's last-seen timestamp for a single DM /
// group-chat grouping id and whether a row exists. A missing row (ok=false)
// means the conversation was never marked seen — callers treat that as
// "everything is unread" under their lookback floor.
func GetLastSeenChat(ctx context.Context, userID uuid.UUID, groupingID string) (lastSeen time.Time, ok bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT user_last_seen FROM last_seen_chat WHERE user_id = $1 AND grp_id = $2`,
		userID, groupingID,
	).Scan(&lastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, false, nil
		}
		helpers.LogErrorWithContext(cctx,
			"models/GetLastSeenChat Failed to read last seen err: %+v", err)
		return time.Time{}, false, err
	}
	return lastSeen, true, nil
}

// GetAllLastSeenChatsForUser returns a map of grp_id → last-seen for every
// conversation the user has a row for, in ONE query. Used by activity triage
// to demote items the user has already viewed.
func GetAllLastSeenChatsForUser(ctx context.Context, userID uuid.UUID) (map[string]time.Time, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT grp_id, user_last_seen FROM last_seen_chat WHERE user_id = $1`, userID)
	if err != nil {
		helpers.LogErrorWithContext(cctx,
			"models/GetAllLastSeenChatsForUser failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]time.Time)
	for rows.Next() {
		var grpID string
		var ts time.Time
		if err := rows.Scan(&grpID, &ts); err != nil {
			return nil, err
		}
		out[grpID] = ts
	}
	return out, rows.Err()
}
