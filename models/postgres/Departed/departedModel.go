// Package models (Departed) reads the people who have left the workspace
// (docs C6-2): deactivated members, never bots or outside people. Every
// statement runs through models/pgexec.
package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Person is someone who has left: who, their address, and when they left.
type Person struct {
	ID     uuid.UUID
	Email  string
	LeftAt time.Time
}

// departedSQL reads a person who has left, by the filter after it.
const departedSQL = `SELECT id, email_id, deleted_at FROM users
	WHERE deleted_at IS NOT NULL AND is_bot = false AND is_external = false`

func scanPerson(r pgexec.Row) (Person, error) {
	var p Person
	err := r.Scan(&p.ID, &p.Email, &p.LeftAt)
	return p, err
}

// ListDeparted is the people who have left, the most recent first.
func ListDeparted(ctx context.Context, limit int) ([]Person, error) {
	out, err := pgexec.Query(ctx, departedSQL+` ORDER BY deleted_at DESC, id LIMIT $1`, scanPerson, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Departed/ListDeparted err: %+v", err)
		return nil, err
	}
	if out == nil {
		out = []Person{}
	}
	return out, nil
}

// Departed is one person who has left, or nil when they haven't (or are a
// bot or an outside person, or don't exist).
func Departed(ctx context.Context, id uuid.UUID) (*Person, error) {
	p, found, err := pgexec.QueryOne(ctx, departedSQL+` AND id = $1`, scanPerson, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Departed/Departed err: %+v", err)
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &p, nil
}

// ActiveMember reports whether id is someone in the workspace now: not
// deactivated, not a bot, not an outside person. Who a handover may go to.
func ActiveMember(ctx context.Context, id uuid.UUID) (bool, string, error) {
	const q = `SELECT email_id FROM users
		WHERE id = $1 AND deleted_at IS NULL AND is_bot = false AND is_external = false`
	email, found, err := pgexec.QueryOne(ctx, q, func(r pgexec.Row) (string, error) {
		var e string
		return e, r.Scan(&e)
	}, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Departed/ActiveMember err: %+v", err)
		return false, "", err
	}
	return found, email, nil
}
