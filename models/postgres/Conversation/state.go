package models

// The conversations index's state, one system_configs row: whether it has been
// filled in from the graph, and whether the last comparison with the graph
// found them alike (business/Conversation's backfill and reconcile; domain/
// Conversation decides from it where access decisions read).

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The system_configs keys.
const (
	// BackfillKey is "done: …" once the index has been filled in from the graph.
	BackfillKey = "conversations_backfill_v1"
	// StateKey is the index's state ("clean|…", "drift|…", "dirty|…").
	StateKey = "conversations_access_state"
	// CompareAfterKey is the latest start a process asked a comparison to
	// begin after (RFC 3339, UTC, to the second): a process trusts no
	// comparison that began before it started (domain/Conversation source.go).
	CompareAfterKey = "conversations_compare_after"
)

// RequestComparison asks for a comparison that begins at or after at, keeping
// the latest time asked for. The stored times are all one width, so their
// order as text (COLLATE "C") is their order in time.
func RequestComparison(ctx context.Context, at time.Time) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		INSERT INTO system_configs (key, value, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = GREATEST(system_configs.value COLLATE "C", EXCLUDED.value COLLATE "C"), updated_at = NOW()`,
		CompareAfterKey, at.UTC().Truncate(time.Second).Format(time.RFC3339))
	return err
}

// ComparisonRequested is the latest time a comparison was asked to begin
// after, and whether there is one (a value that isn't a time is none).
func ComparisonRequested(ctx context.Context) (time.Time, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var v string
	err := db().QueryRowContext(cctx, `SELECT value FROM system_configs WHERE key = $1`, CompareAfterKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	at, perr := time.Parse(time.RFC3339, v)
	if perr != nil {
		return time.Time{}, false, nil
	}
	return at, true, nil
}

// ReadState is the state, and whether there is one.
func ReadState(ctx context.Context) (string, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var v string
	err := db().QueryRowContext(cctx, `SELECT value FROM system_configs WHERE key = $1`, StateKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetState writes the state whatever it was.
func SetState(ctx context.Context, value string) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		INSERT INTO system_configs (key, value, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()`, StateKey, value)
	return err
}

// SwapState writes to only while the state is still from ("" for none), and
// says whether it did: a comparison that began before a write failed must not
// report the index clean over that failure.
func SwapState(ctx context.Context, from, to string) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var res sql.Result
	var err error
	if from == "" {
		res, err = db().ExecContext(cctx, `
			INSERT INTO system_configs (key, value, updated_at) VALUES ($1, $2, NOW())
			ON CONFLICT (key) DO NOTHING`, StateKey, to)
	} else {
		res, err = db().ExecContext(cctx, `
			UPDATE system_configs SET value = $2, updated_at = NOW() WHERE key = $1 AND value = $3`, StateKey, to, from)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
