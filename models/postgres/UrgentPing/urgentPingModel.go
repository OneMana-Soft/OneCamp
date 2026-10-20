// Package models stores urgent pings ("notify anyway"): who pinged whom
// through a pause or quiet hours, and when, so a pair gets at most a few a day.
package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Claim records a ping from sender to recipient when fewer than limit were
// sent since then, and says whether it did. The count and the write hold the
// pair's transaction lock: counted apart, two taps at once both saw none and
// both went through.
func Claim(ctx context.Context, sender, recipient uuid.UUID, since time.Time, limit int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('urgent_ping:' || $1::text || ':' || $2::text))`,
		sender, recipient); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM urgent_pings WHERE sender_id = $1 AND recipient_id = $2 AND created_at > $3`,
		sender, recipient, since).Scan(&n); err != nil {
		return false, err
	}
	if n >= limit {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO urgent_pings (sender_id, recipient_id) VALUES ($1, $2)`, sender, recipient); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
