// Package models (ProjectRate) stores what a project's time is billed at
// (migration 189): its currency, a rate for everyone, and a rate per person.
// A billing cleared and a person's rate taken away stay, marked by who did it
// and when (V-184 head 4a); every read takes live rows only.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
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
		`SELECT currency, default_rate_cents, updated_at FROM project_billing WHERE project_uuid = $1 AND `+softDelete.Live(""), project).
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
// Each person's rate is written or taken away on its own, in one transaction;
// one taken away stays, marked by, and a rate given again brings its row back.
// Billing a cleared project again brings its row back too; the rates its
// people had stay as the clear left them. The answer is the new UpdatedAt.
func Set(project uuid.UUID, currency string, defaultCents int64, people map[uuid.UUID]int64, read time.Time, by uuid.UUID) (time.Time, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var at time.Time
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var err error
		at, err = pgexec.Scalar[time.Time](ctx, `SELECT updated_at FROM project_billing WHERE project_uuid = $1 AND `+softDelete.Live("")+` FOR UPDATE`, project)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !read.IsZero() {
				return ErrChanged
			}
			// A first save, or one after a clear, whose row comes back. Two at
			// once: the second finds the first's row live, and writes nothing.
			at, err = pgexec.Scalar[time.Time](ctx, `
				INSERT INTO project_billing (project_uuid, currency, default_rate_cents, updated_by) VALUES ($1, $2, $3, $4)
				ON CONFLICT (project_uuid) DO UPDATE SET currency = EXCLUDED.currency, default_rate_cents = EXCLUDED.default_rate_cents,
				  updated_by = EXCLUDED.updated_by, updated_at = NOW(), deleted_at = NULL, deleted_by = NULL
				WHERE project_billing.deleted_at IS NOT NULL
				RETURNING updated_at`, project, currency, defaultCents, by)
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
				WHERE project_uuid = $1 AND `+softDelete.Live("")+` RETURNING updated_at`, project, currency, defaultCents, by)
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
				ON CONFLICT (project_uuid, user_uuid) DO UPDATE SET rate_cents = EXCLUDED.rate_cents, deleted_at = NULL, deleted_by = NULL`, project, id, cents); err != nil {
				return err
			}
		}
		for id := range had {
			if _, kept := people[id]; kept {
				continue
			}
			if _, err := pgexec.Exec(ctx, `UPDATE project_person_rates SET deleted_at = NOW(), deleted_by = $3 WHERE project_uuid = $1 AND user_uuid = $2 AND `+softDelete.Live(""),
				project, id, softDelete.By(by)); err != nil {
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
	return pgexec.Pairs[uuid.UUID, int64](ctx, `SELECT user_uuid, rate_cents FROM project_person_rates WHERE project_uuid = $1 AND `+softDelete.Live(""), project)
}

// Clear stops billing a project, by whom: its reports show hours only. Its
// billing and its people's rates stay, marked by (V-184 head 4a), so what the
// project was billed at, and who stopped it and when, stay on record; no read
// sees them, and a save made from the rates read before finds them changed.
func Clear(project, by uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		for _, q := range []string{
			`UPDATE project_billing SET deleted_at = NOW(), deleted_by = $2 WHERE project_uuid = $1 AND ` + softDelete.Live(""),
			`UPDATE project_person_rates SET deleted_at = NOW(), deleted_by = $2 WHERE project_uuid = $1 AND ` + softDelete.Live(""),
		} {
			if _, err := pgexec.Exec(ctx, q, project, softDelete.By(by)); err != nil {
				return err
			}
		}
		return nil
	})
}
