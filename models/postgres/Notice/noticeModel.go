// Package models (Notice) keeps the one-time notices each person has seen
// (migration 320): a notice seen on one device is seen on all of them.
package models

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ErrNoDatabase is a read or write with no database.
var ErrNoDatabase = errors.New("notices: not connected")

func connected() bool {
	return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil
}

// SeenOf is which of the notices the person has seen, in one read.
func SeenOf(ctx context.Context, userID uuid.UUID, notices []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(notices) == 0 {
		return out, nil
	}
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT notice FROM user_notices_seen WHERE user_id = $1 AND notice = ANY($2)`, userID, notices)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// MarkSeen records that the person has seen the notice; seeing it again
// changes nothing.
func MarkSeen(ctx context.Context, userID uuid.UUID, notice string) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`INSERT INTO user_notices_seen (user_id, notice) VALUES ($1, $2) ON CONFLICT (user_id, notice) DO NOTHING`, userID, notice)
	return err
}
