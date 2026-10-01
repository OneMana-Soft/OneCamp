// Package models (AgentNote) records the daily note a member's AI teammate
// leaves in their DM (migration 165).
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

// ClaimDay marks day as this member's note day and reports whether it was
// theirs to claim: false when a note already went out that day (another tab,
// another instance) or the member has turned notes off. One statement, so two
// tabs opening at once cannot both send one.
func ClaimDay(ctx context.Context, userID uuid.UUID, day time.Time) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var got uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO agent_notes (user_id, last_day) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET last_day = EXCLUDED.last_day, updated_at = NOW()
		WHERE agent_notes.opted_out = false
		  AND (agent_notes.last_day IS NULL OR agent_notes.last_day < EXCLUDED.last_day)
		RETURNING user_id`, userID, day.Format("2006-01-02")).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AgentNote ClaimDay err: %+v", err)
		return false, err
	}
	return true, nil
}

// Enabled reports whether a member still wants the note. No row means yes.
func Enabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var optedOut bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT opted_out FROM agent_notes WHERE user_id = $1`, userID).Scan(&optedOut)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !optedOut, nil
}

// SetEnabled records a member's choice.
func SetEnabled(ctx context.Context, userID uuid.UUID, enabled bool) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO agent_notes (user_id, opted_out) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET opted_out = EXCLUDED.opted_out, updated_at = NOW()`,
		userID, !enabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AgentNote SetEnabled err: %+v", err)
	}
	return err
}

// LeftOn reports whether the member's note day is day.
func LeftOn(ctx context.Context, userID uuid.UUID, day time.Time) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var last sql.NullTime
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT last_day FROM agent_notes WHERE user_id = $1 AND opted_out = false`, userID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return last.Valid && last.Time.Format("2006-01-02") == day.Format("2006-01-02"), nil
}
