// Package models (Follow) stores follows, the inbox's notices, and who a
// thing's body mentions (migration 210, business/Follow). Every row names a
// kind of thing and its id, so docs, channels and events share the tables.
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

// The follow modes, as stored.
const (
	ModeAll      = "all"
	ModeMentions = "mentions"
	ModeNone     = "none"
)

// ValidMode is whether m is a mode a row may hold.
func ValidMode(m string) bool { return m == ModeAll || m == ModeMentions || m == ModeNone }

// Follow is one person's choice about one thing.
type Follow struct {
	UserUUID     uuid.UUID `json:"user_uuid"`
	ResourceKind string    `json:"resource_kind"`
	ResourceUUID string    `json:"resource_uuid"`
	Mode         string    `json:"mode"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ErrNotFound is a choice the person never made.
var ErrNotFound = errors.New("follow not found")

// ErrBadMode is a mode outside all, mentions and none.
var ErrBadMode = errors.New("follow: the mode is all, mentions or none")

// GetFollow is the person's choice about the thing, ErrNotFound if they made none.
func GetFollow(ctx context.Context, userUUID uuid.UUID, kind, resourceUUID string) (*Follow, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var f Follow
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT user_uuid, resource_kind, resource_uuid, mode, created_at, updated_at
		   FROM resource_follows WHERE user_uuid = $1 AND resource_kind = $2 AND resource_uuid = $3`,
		userUUID, kind, resourceUUID).Scan(&f.UserUUID, &f.ResourceKind, &f.ResourceUUID, &f.Mode, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow GetFollow err: %+v", err)
		return nil, err
	}
	return &f, nil
}

// SetFollow records the person's choice, replacing any earlier one.
func SetFollow(ctx context.Context, userUUID uuid.UUID, kind, resourceUUID, mode string) error {
	if !ValidMode(mode) {
		return ErrBadMode
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`INSERT INTO resource_follows (user_uuid, resource_kind, resource_uuid, mode, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 ON CONFLICT (user_uuid, resource_kind, resource_uuid) DO UPDATE SET mode = EXCLUDED.mode, updated_at = NOW()`,
		userUUID, kind, resourceUUID, mode)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow SetFollow err: %+v", err)
	}
	return err
}

// ClearFollow forgets the person's choice: the thing's defaults apply again.
func ClearFollow(ctx context.Context, userUUID uuid.UUID, kind, resourceUUID string) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM resource_follows WHERE user_uuid = $1 AND resource_kind = $2 AND resource_uuid = $3`,
		userUUID, kind, resourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow ClearFollow err: %+v", err)
	}
	return err
}

// FollowModes is every choice made about the thing, by person: those with no
// row aren't in it.
func FollowModes(ctx context.Context, kind, resourceUUID string) (map[string]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT user_uuid, mode FROM resource_follows WHERE resource_kind = $1 AND resource_uuid = $2`,
		kind, resourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow FollowModes err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var u uuid.UUID
		var mode string
		if err := rows.Scan(&u, &mode); err != nil {
			return nil, err
		}
		out[u.String()] = mode
	}
	return out, rows.Err()
}

// ForgetResources removes every follow, notice and mention of these things,
// and the record of their mentions' first keeping, for when they are gone for
// good. Each table is tried on its own.
func ForgetResources(ctx context.Context, kind string, resourceUUIDs []string) error {
	if len(resourceUUIDs) == 0 {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var first error
	for _, q := range []string{
		`DELETE FROM resource_follows WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		`DELETE FROM resource_notices WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		`DELETE FROM resource_mentions WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		`DELETE FROM resource_mention_syncs WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
	} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(c, q, kind, resourceUUIDs); err != nil {
			helpers.LogErrorWithContext(ctx, "models/Follow ForgetResources err: %+v", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}
