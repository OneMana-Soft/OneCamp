// Package models stores urgent pings ("notify anyway"): who pinged whom
// through a pause or quiet hours, and when, so a pair gets at most a few a day.
package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Claim records a ping from sender to recipient when fewer than limit were
// sent since then, and says whether it did. The count and the write hold the
// pair's transaction lock: counted apart, two taps at once both saw none and
// both went through.
func Claim(ctx context.Context, sender, recipient uuid.UUID, since time.Time, limit int) (bool, error) {
	claimed := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('urgent_ping:' || $1::text || ':' || $2::text))`,
			sender, recipient); err != nil {
			return err
		}
		n, err := pgexec.Scalar[int](ctx,
			`SELECT COUNT(*) FROM urgent_pings WHERE sender_id = $1 AND recipient_id = $2 AND created_at > $3`,
			sender, recipient, since)
		if err != nil || n >= limit {
			return err
		}
		if _, err := pgexec.Exec(ctx, `INSERT INTO urgent_pings (sender_id, recipient_id) VALUES ($1, $2)`, sender, recipient); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed && err == nil, err
}
