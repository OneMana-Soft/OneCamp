// Package models (CheckIn) stores automatic check-ins (migration 190).
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// CheckIn is one recurring question a channel asks its people.
type CheckIn struct {
	Id           uuid.UUID
	ChannelUUID  uuid.UUID
	Question     string
	Days         int
	AtMinute     int
	TZ           string
	CreatedBy    uuid.UUID
	Paused       bool
	NextRunAt    *time.Time
	LastPostUUID *uuid.UUID
	LastAskedAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MaxPerChannel is how many check-ins one channel can have.
const MaxPerChannel = 20

const columns = `id, channel_uuid, question, days, at_minute, tz, created_by, paused, next_run_at, last_post_uuid, last_asked_at, created_at, updated_at`

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (*CheckIn, error) {
	var c CheckIn
	err := row.Scan(&c.Id, &c.ChannelUUID, &c.Question, &c.Days, &c.AtMinute, &c.TZ, &c.CreatedBy, &c.Paused,
		&c.NextRunAt, &c.LastPostUUID, &c.LastAskedAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// ErrTooMany is a channel with the most check-ins it can have.
var ErrTooMany = errors.New("channel has the most check-ins it can have")

// Create stores a check-in with its first time, refusing a channel's
// twenty-first. One statement: a check-in never exists without a time.
func Create(c CheckIn) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO checkins (channel_uuid, question, days, at_minute, tz, created_by, next_run_at)
		SELECT $1, $2, $3, $4, $5, $6, $8
		 WHERE (SELECT COUNT(*) FROM checkins WHERE channel_uuid = $1 AND deleted_at IS NULL) < $7
		RETURNING `+columns, c.ChannelUUID, c.Question, c.Days, c.AtMinute, c.TZ, c.CreatedBy, MaxPerChannel, c.NextRunAt))
	if err == nil && out == nil {
		return nil, ErrTooMany
	}
	return out, err
}

// Update changes what a check-in asks and when, with its next time (kept
// empty while it's paused); nil when it is gone.
func Update(id uuid.UUID, question string, days, atMinute int, tz string, next time.Time) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE checkins SET question = $2, days = $3, at_minute = $4, tz = $5,
		       next_run_at = CASE WHEN paused THEN NULL ELSE $6::timestamptz END, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+columns, id, question, days, atMinute, tz, next))
}

// SetPaused pauses a check-in (no next time) or resumes it at next.
func SetPaused(id uuid.UUID, paused bool, next *time.Time) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE checkins SET paused = $2, next_run_at = CASE WHEN $2 THEN NULL ELSE $3::timestamptz END, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+columns, id, paused, next))
}

// Claim takes a due time for one worker: it moves the check-in's next time
// from due to next, and only one worker's UPDATE can find it still at due.
func Claim(id uuid.UUID, due, next time.Time) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE checkins SET next_run_at = $3
		 WHERE id = $1 AND next_run_at = $2 AND NOT paused AND deleted_at IS NULL`, id, due, next)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Asked records the post an occurrence was asked in.
func Asked(id, postUUID uuid.UUID, at time.Time) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE checkins SET last_post_uuid = $2, last_asked_at = $3 WHERE id = $1`, id, postUUID, at)
	return err
}

// Delete removes a check-in; its pending job then finds nothing to do.
func Delete(id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE checkins SET deleted_at = NOW(), next_run_at = NULL WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Get is one live check-in, or nil.
func Get(id uuid.UUID) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+columns+` FROM checkins WHERE id = $1 AND deleted_at IS NULL`, id))
}

// ForChannel is a channel's live check-ins, oldest first.
func ForChannel(channel uuid.UUID) ([]CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+columns+` FROM checkins WHERE channel_uuid = $1 AND deleted_at IS NULL ORDER BY created_at`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckIn{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Due is the live, running check-ins whose next time has come, soonest first.
func Due(now time.Time, limit int) ([]CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT `+columns+` FROM checkins
		 WHERE deleted_at IS NULL AND NOT paused AND next_run_at <= $1
		 ORDER BY next_run_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckIn{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
