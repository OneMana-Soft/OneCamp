package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type Channel struct {
	Id        uuid.UUID
	Name      string    `json:"ch_name"`
	IsPrivate bool      `json:"ch_private"`
	CreatedBy uuid.UUID `json:"user"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

func CreateChannel(query string, channelName string, userUUID uuid.UUID, channelUUID uuid.UUID, channelPrivate bool) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		channelUUID,
		channelName,
		channelPrivate,
		userUUID,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChannel Failed to create new user err: %+v",
			err)
		return
	}

	return
}

func CheckIfChannelExist(query string, uname string) (exist bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uname).Scan(&exist)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CheckIfChannel Failed to check if channel handle exist err: %+v",
			err)
		return
	}

	return
}

func GetChannelByName(query string, channelName string) (channelInfo *Channel, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var channelData Channel
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, channelName)
	err = row.Scan(
		&channelData.Id,
		&channelData.Name,
		&channelData.CreatedBy,
		&channelData.IsPrivate,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			helpers.LogErrorWithContext(ctx,
				"models/GetChannelByName Failed to get channel err: %+v",
				err)
		}
		return
	}

	if createdAt.Valid {
		channelData.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		channelData.UpdatedAt = updatedAt.Time
	}
	if deletedAt.Valid {
		channelData.DeletedAt = deletedAt.Time
	}

	return &channelData, nil
}

func GetChannelByUUID(query *string, channelUUID uuid.UUID) (channelInfo *Channel, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var channelData Channel
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, *query, channelUUID)
	err = row.Scan(
		&channelData.Id,
		&channelData.Name,
		&channelData.CreatedBy,
		&channelData.IsPrivate,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetChannelByChannelName Failed to get channel err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		channelData.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		channelData.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		channelData.DeletedAt = deletedAt.Time
	}

	return &channelData, nil
}

// SetChannelPostPolicy updates the channel posting policy. Returns
// sql.ErrNoRows-equivalent via affected-rows == 0 (handled by the caller).
func SetChannelPostPolicy(channelUUID uuid.UUID, policy string) (affected int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE channels SET post_policy = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`,
		policy, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetChannelPostPolicy failed: %+v", err)
		return 0, err
	}
	affected, _ = res.RowsAffected()
	return affected, nil
}

func UpdateChannelInfo(query string, channelName string, channelPrivate bool, currentTime time.Time, channelUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		channelName,
		channelPrivate,
		currentTime,
		channelUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChannelInfo Failed to update channel info err: %+v",
			err)
		return
	}

	return
}

func SoftDeleteChannelInfo(query string, channelHandle string, channelPrivate bool, currentTime time.Time, channelUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		channelHandle,
		channelPrivate,
		currentTime,
		channelUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/SoftDeleteChannelInfo Failed to soft delete channel info err: %+v",
			err)
		return
	}

	return
}

// GetChannelAITokenCap returns a channel's per-day AI token cap (0 = no cap).
// Read-only and cheap (PK lookup); a missing/deleted channel reports 0 so AI is
// never blocked by a lookup miss (fail-open, consistent with the budget layer).
func GetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var cap int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT max_daily_ai_tokens FROM channels WHERE id = $1 AND deleted_at IS NULL`,
		channelUUID).Scan(&cap)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetChannelAITokenCap failed: %+v", err)
		return 0, err
	}
	return cap, nil
}

// SetChannelAITokenCap sets a channel's per-day AI token cap (0 = no cap).
// Returns affected-rows so the caller can detect a missing channel.
func SetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID, cap int) (affected int64, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if cap < 0 {
		cap = 0
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE channels SET max_daily_ai_tokens = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`,
		cap, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetChannelAITokenCap failed: %+v", err)
		return 0, err
	}
	affected, _ = res.RowsAffected()
	return affected, nil
}

// GetChannelAIModel returns a channel's pinned AI model id (the admin-allowlist
// entry), or nil when the channel has no override. Read-only, PK lookup; a
// missing/deleted channel reports nil so AI is never blocked by a lookup miss.
func GetChannelAIModel(ctx context.Context, channelUUID uuid.UUID) (*uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var id uuid.NullUUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT ai_model_id FROM channels WHERE id = $1 AND deleted_at IS NULL`,
		channelUUID).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetChannelAIModel failed: %+v", err)
		return nil, err
	}
	if id.Valid {
		v := id.UUID
		return &v, nil
	}
	return nil, nil
}

// SetChannelAIModel pins (or, with a nil id, clears) a channel's default AI
// model. Returns affected-rows so the caller can detect a missing channel.
func SetChannelAIModel(ctx context.Context, channelUUID uuid.UUID, modelID *uuid.UUID) (affected int64, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var arg interface{}
	if modelID != nil {
		arg = *modelID
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE channels SET ai_model_id = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`,
		arg, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetChannelAIModel failed: %+v", err)
		return 0, err
	}
	affected, _ = res.RowsAffected()
	return affected, nil
}

// GetChannelNamesByUUIDs resolves a set of channel UUIDs to their names in one
// query, for the admin per-channel usage breakdown. Missing/deleted channels
// are simply absent from the map. Read-only and best-effort.
func GetChannelNamesByUUIDs(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT id, ch_name FROM channels WHERE id = ANY($1) AND deleted_at IS NULL`,
		pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetChannelNamesByUUIDs failed: %+v", err)
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if scanErr := rows.Scan(&id, &name); scanErr != nil {
			return out, scanErr
		}
		out[id] = name
	}
	return out, rows.Err()
}

// HardDeleteChannel removes a channel row outright. Compensation only.
//
// Everything else in the product soft-deletes, and should: history, posts and audit all expect
// the row to survive. This exists for one case — a channel was inserted here and the matching
// Dgraph node could not be created, so the row describes a channel no part of the product can
// see. A soft delete would not do: ch_name carries a plain UNIQUE constraint that a
// soft-deleted row still occupies, so the name would stay reserved by something invisible and
// the user could never retry with it.
//
// It is safe in that window and only in that window: channels(id) is referenced by posts,
// webhooks, last_seen_channel and users_channel_notification with no ON DELETE, so this fails
// rather than cascades once a channel has any of them. The compensation path runs before any of
// those are written. A failure here is reported, never swallowed — that is the case that leaves
// the orphan.
func HardDeleteChannel(query string, channelUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteChannel Failed to delete channel row err: %+v", err)
		return
	}
	return
}

// RestoreChannelRow writes a channel's previous name, privacy and deletion timestamp back.
// Compensation only, for a failed update whose Postgres half already landed.
//
// deletedAt is a pointer so the caller can restore SQL NULL — the difference between a channel
// that was archived before the failed update and one that was not. Passing a zero time instead
// would silently archive a live channel.
func RestoreChannelRow(query string, channelName string, channelPrivate bool, deletedAt *time.Time, updatedAt time.Time, channelUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx, query, channelName, channelPrivate, deletedAt, updatedAt, channelUUID,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/RestoreChannelRow Failed to restore channel row err: %+v", err)
		return
	}
	return
}

func GetChannelByHandle(query *string, channelHandle *string) (channelInfo *Channel, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var channelData Channel
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, *query, *channelHandle)
	err = row.Scan(
		&channelData.Id,
		&channelData.Name,
		&channelData.CreatedBy,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetChannelByChannelName Failed to get channel err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		channelData.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		channelData.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		channelData.DeletedAt = deletedAt.Time
	}

	return &channelData, nil
}

// PublicLiveChannels returns the live public channels among ids, or every one
// (up to limit) when ids is nil, by name. A channel that is missing, archived
// or private is left out: these are the channels anyone may join themselves.
func PublicLiveChannels(ctx context.Context, ids []string, limit int) ([]Channel, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var filter interface{}
	if ids != nil {
		filter = pq.Array(ids)
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT id, ch_name FROM channels
		WHERE ch_private = false AND deleted_at IS NULL
		  AND ($1::uuid[] IS NULL OR id = ANY($1::uuid[]))
		ORDER BY ch_name
		LIMIT $2`, filter, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PublicLiveChannels err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch.Id, &ch.Name); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}
