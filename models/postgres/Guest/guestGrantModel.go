// Package models (Guest) is the Postgres data-access layer for guest access
// grants (migration 97). Only the SHA-256 hash of a link token is stored;
// lookups are by hash. The business layer generates tokens, hashes them, and
// enforces the workspace guest-access policy.
//
// A guest grant authorizes an external person (no OneCamp account) to reach
// exactly one resource for a bounded time. Guests are never written to the
// users table, so they cannot leak into rosters, search, memory, or mentions.
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

// Resource types and capabilities.
const (
	ResourceMeeting = "meeting"
	ResourceDoc     = "doc"
	ResourceBoard   = "board"
	ResourceTable   = "table"
	ResourceChannel = "channel"

	CapabilityJoin    = "join"
	CapabilityView    = "view"
	CapabilityComment = "comment"
	// CapabilityPost lets a channel guest write as well as read.
	CapabilityPost = "post"
)

// GuestGrant mirrors a row of guest_grants. The raw token is never stored.
type GuestGrant struct {
	Id           uuid.UUID `json:"id"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Capability   string    `json:"capability"`
	CreatedBy    uuid.UUID `json:"created_by"`
	// ExpiresAt nil means the grant lasts until somebody revokes it. A sentinel
	// far-future date was the alternative and would read as a real expiry in the
	// admin's grant list.
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type scanner interface {
	Scan(dest ...any) error
}

const grantColumns = `id, resource_type, resource_id, capability, created_by, expires_at, revoked_at, created_at`

func scanGrant(s scanner) (*GuestGrant, error) {
	var g GuestGrant
	var revokedAt, expiresAt sql.NullTime
	if err := s.Scan(&g.Id, &g.ResourceType, &g.ResourceID, &g.Capability,
		&g.CreatedBy, &expiresAt, &revokedAt, &g.CreatedAt); err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		g.RevokedAt = &revokedAt.Time
	}
	if expiresAt.Valid {
		g.ExpiresAt = &expiresAt.Time
	}
	return &g, nil
}

// CreateGrant inserts a new grant (token hash + scope) and returns its id.
// expiresAt nil creates a grant that lasts until it is revoked.
func CreateGrant(ctx context.Context, tokenHash []byte, resourceType, resourceID, capability string, createdBy uuid.UUID, expiresAt *time.Time) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, tokenHash, resourceType, resourceID, capability, createdBy, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CreateGrant err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetActiveByHash returns the active (non-revoked, non-expired) grant for a
// token hash, or (nil, nil) if none. Used on every guest request.
func GetActiveByHash(ctx context.Context, tokenHash []byte) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + grantColumns + ` FROM guest_grants
		WHERE token_hash=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/GetActiveByHash err: %+v", err)
		return nil, err
	}
	return g, nil
}

// GetByID returns a grant by id regardless of state (admin lookup).
func GetByID(ctx context.Context, id uuid.UUID) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + grantColumns + ` FROM guest_grants WHERE id=$1`
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/GetByID err: %+v", err)
		return nil, err
	}
	return g, nil
}

// Revoke marks a grant revoked (idempotent). Admin-scoped: any admin may
// revoke any grant, so there is no created_by constraint here (the controller
// enforces the admin gate).
func Revoke(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE guest_grants SET revoked_at=NOW() WHERE id=$1 AND revoked_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/Revoke err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ResourceExists reports whether ANY guest grant (active or not) exists for a
// resource id. Used by the LiveKit webhook router to decide whether a `meet-`
// room belongs to this instance (mirrors the DM "any participant in our DB"
// ownership check for a shared-LiveKit deployment).
func ResourceExists(ctx context.Context, resourceID string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT EXISTS (SELECT 1 FROM guest_grants WHERE resource_id=$1)`
	var exists bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, resourceID).Scan(&exists); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ResourceExists err: %+v", err)
		return false, err
	}
	return exists, nil
}

// Used by the admin guest-grants view.
func ListActive(ctx context.Context) ([]*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + grantColumns + ` FROM guest_grants
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at DESC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ListActive err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*GuestGrant
	for rows.Next() {
		g, scanErr := scanGrant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
