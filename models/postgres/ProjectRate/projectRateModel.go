// Package models (ProjectRate) stores what a project's time is billed at
// (migration 189): its currency, a rate for everyone, and a rate per person.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Billing is a project's rates, per hour in the currency's minor unit.
type Billing struct {
	Currency         string
	DefaultRateCents int64
	People           map[uuid.UUID]int64
	UpdatedAt        time.Time
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

// Get is a project's billing, or nil when it has none.
func Get(project uuid.UUID) (*Billing, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	b := Billing{People: map[uuid.UUID]int64{}}
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT currency, default_rate_cents, updated_at FROM project_billing WHERE project_uuid = $1`, project).
		Scan(&b.Currency, &b.DefaultRateCents, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT user_uuid, rate_cents FROM project_person_rates WHERE project_uuid = $1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var cents int64
		if err := rows.Scan(&id, &cents); err != nil {
			return nil, err
		}
		b.People[id] = cents
	}
	return &b, rows.Err()
}

// Set replaces a project's billing: its currency, its rate for everyone, and
// the people with their own rate, in one transaction.
func Set(project uuid.UUID, currency string, defaultCents int64, people map[uuid.UUID]int64, by uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO project_billing (project_uuid, currency, default_rate_cents, updated_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_uuid) DO UPDATE SET currency = EXCLUDED.currency, default_rate_cents = EXCLUDED.default_rate_cents,
		       updated_by = EXCLUDED.updated_by, updated_at = NOW()`, project, currency, defaultCents, by); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_person_rates WHERE project_uuid = $1`, project); err != nil {
		return err
	}
	for id, cents := range people {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_person_rates (project_uuid, user_uuid, rate_cents) VALUES ($1, $2, $3)`,
			project, id, cents); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Clear stops billing a project: its rates go, and its reports show hours only.
func Clear(project uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM project_billing WHERE project_uuid = $1`, project)
	return err
}
