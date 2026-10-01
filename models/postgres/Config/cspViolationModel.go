package models

// Aggregated Content-Security-Policy violations.
//
// One row per distinct (directive, blocked origin, document path, disposition),
// with a count. See migrations/146 for why this aggregates rather than appends.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// errNoDatabase is returned rather than panicking when the pool is not up. See
// RecordCSPViolation for why that distinction matters here specifically.
var errNoDatabase = errors.New("csp violations: database not initialised")

// CSPViolation is one distinct thing the policy would block, and how often.
type CSPViolation struct {
	Id            uuid.UUID `json:"id"`
	Directive     string    `json:"directive"`
	BlockedOrigin string    `json:"blocked_origin"`
	DocumentPath  string    `json:"document_path"`
	Disposition   string    `json:"disposition"`
	TimesSeen     int64     `json:"times_seen"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
}

// RecordCSPViolation upserts one violation, incrementing its count.
//
// The whole aggregation depends on this being an upsert rather than an insert.
// ON CONFLICT is what turns a stream of reports, one per page load, into a set
// of distinct problems an operator can read in a minute.
func RecordCSPViolation(ctx context.Context, directive, blockedOrigin, documentPath, disposition string) error {
	// The only caller is an OPEN endpoint that must answer 204 to everything,
	// including when the database is not there. A nil handle would panic, chi's
	// recoverer would turn that into a 500, and a public endpoint would start
	// announcing that something internal had broken. Recording a violation is
	// never worth that.
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return errNoDatabase
	}

	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		INSERT INTO csp_violations
			(id, directive, blocked_origin, document_path, disposition)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (directive, blocked_origin, document_path, disposition)
		DO UPDATE SET times_seen = csp_violations.times_seen + 1, last_seen = now()`

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		uuid.New(), directive, blockedOrigin, documentPath, disposition)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RecordCSPViolation err: %+v", err)
	}
	return err
}

// ListCSPViolations returns distinct violations, most recently seen first.
//
// Recency rather than count on purpose: volume does not decide priority. A
// browser extension can produce thousands of irrelevant reports while a rare
// failure on a sign-in path produces three, and the three are what matter.
func ListCSPViolations(ctx context.Context, limit int) ([]*CSPViolation, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, errNoDatabase
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 500 {
		limit = 200
	}

	const q = `SELECT id, directive, blocked_origin, document_path, disposition,
			times_seen, first_seen, last_seen
		FROM csp_violations ORDER BY last_seen DESC LIMIT $1`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListCSPViolations err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*CSPViolation
	for rows.Next() {
		var v CSPViolation
		if err := rows.Scan(&v.Id, &v.Directive, &v.BlockedOrigin, &v.DocumentPath,
			&v.Disposition, &v.TimesSeen, &v.FirstSeen, &v.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// ClearCSPViolations empties the table.
//
// The point of a validation window is to fix something and see whether it stops
// happening. Without a reset an operator cannot tell a violation they just fixed
// from one still occurring, because both have an old first_seen and a count that
// only ever grows.
func ClearCSPViolations(ctx context.Context) error {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return errNoDatabase
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `DELETE FROM csp_violations`)
	return err
}
