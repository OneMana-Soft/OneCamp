// Package models (ProjectRate) stores what a project's time is billed at
// (migration 189): its currency, a rate for everyone, and a rate per person.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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
	var b Billing
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT currency, default_rate_cents, updated_at FROM project_billing WHERE project_uuid = $1`, project).
		Scan(&b.Currency, &b.DefaultRateCents, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b.People, err = personRates(ctx, project); err != nil {
		return nil, err
	}
	return &b, nil
}

// ErrChanged is a save of rates that changed after the saver read them: by
// someone else, or made or cleared since.
var ErrChanged = errors.New("the rates changed since they were read")

// Set makes a project's billing what was asked: its currency, its rate for
// everyone, and the people with their own rate. read is the UpdatedAt of the
// rates the asker read, zero when it read none; when they've changed since,
// nothing is written and the answer is ErrChanged, so a save made from a read
// that failed, or one that crossed another admin's, can't wipe the rates.
// Each person's rate is written or removed on its own, in one transaction.
// The answer is the new UpdatedAt.
func Set(project uuid.UUID, currency string, defaultCents int64, people map[uuid.UUID]int64, read time.Time, by uuid.UUID) (time.Time, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var at time.Time
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var err error
		at, err = pgexec.Scalar[time.Time](ctx, `SELECT updated_at FROM project_billing WHERE project_uuid = $1 FOR UPDATE`, project)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !read.IsZero() {
				return ErrChanged
			}
			// Two first saves at once: the second finds the first's row.
			at, err = pgexec.Scalar[time.Time](ctx, `
				INSERT INTO project_billing (project_uuid, currency, default_rate_cents, updated_by) VALUES ($1, $2, $3, $4)
				ON CONFLICT (project_uuid) DO NOTHING RETURNING updated_at`, project, currency, defaultCents, by)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrChanged
			}
		case err != nil:
			return err
		case !at.Equal(read):
			return ErrChanged
		default:
			at, err = pgexec.Scalar[time.Time](ctx, `
				UPDATE project_billing SET currency = $2, default_rate_cents = $3, updated_by = $4, updated_at = NOW()
				WHERE project_uuid = $1 RETURNING updated_at`, project, currency, defaultCents, by)
		}
		if err != nil {
			return err
		}
		had, err := personRates(ctx, project)
		if err != nil {
			return err
		}
		for id, cents := range people {
			if was, ok := had[id]; ok && was == cents {
				continue
			}
			if _, err := pgexec.Exec(ctx, `
				INSERT INTO project_person_rates (project_uuid, user_uuid, rate_cents) VALUES ($1, $2, $3)
				ON CONFLICT (project_uuid, user_uuid) DO UPDATE SET rate_cents = EXCLUDED.rate_cents`, project, id, cents); err != nil {
				return err
			}
		}
		for id := range had {
			if _, kept := people[id]; kept {
				continue
			}
			if _, err := pgexec.Exec(ctx, `DELETE FROM project_person_rates WHERE project_uuid = $1 AND user_uuid = $2`, project, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return at, nil
}

// personRates is a project's people with their own rate, read in the
// transaction ctx carries, if any.
func personRates(ctx context.Context, project uuid.UUID) (map[uuid.UUID]int64, error) {
	return pgexec.Pairs[uuid.UUID, int64](ctx, `SELECT user_uuid, rate_cents FROM project_person_rates WHERE project_uuid = $1`, project)
}

// Clear stops billing a project: its rates go, and its reports show hours only.
func Clear(project uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM project_billing WHERE project_uuid = $1`, project)
	return err
}
