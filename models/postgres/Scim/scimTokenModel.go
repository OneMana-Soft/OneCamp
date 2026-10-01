// Package models (Scim) is the Postgres data-access layer for SCIM provisioning credentials
// (migration 142). Only the SHA-256 hash of a credential is stored; lookups are by hash.
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

// ScimToken mirrors a row of scim_tokens. The secret itself is never stored.
type ScimToken struct {
	Id          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	TokenPrefix string    `json:"token_prefix"`
	// CreatedBy is AUDIT ONLY and nullable. The credential authorises the workspace's directory
	// integration, not a person, so its validity deliberately does not depend on this user still
	// existing or still being active — see the migration for the outage that design avoids.
	CreatedBy  *uuid.UUID `json:"created_by,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type scanner interface {
	Scan(dest ...any) error
}

const scimTokenColumns = `id, name, token_prefix, created_by, last_used_at, expires_at, revoked_at, created_at, updated_at`

func scanScimToken(s scanner) (*ScimToken, error) {
	var t ScimToken
	var createdBy uuid.NullUUID
	var lastUsedAt, expiresAt, revokedAt sql.NullTime
	if err := s.Scan(&t.Id, &t.Name, &t.TokenPrefix, &createdBy,
		&lastUsedAt, &expiresAt, &revokedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	if createdBy.Valid {
		t.CreatedBy = &createdBy.UUID
	}
	if lastUsedAt.Valid {
		t.LastUsedAt = &lastUsedAt.Time
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	if revokedAt.Valid {
		t.RevokedAt = &revokedAt.Time
	}
	return &t, nil
}

// CreateScimToken inserts a credential row and returns its id.
func CreateScimToken(ctx context.Context, name, tokenHash, tokenPrefix string, createdBy uuid.UUID, expiresAt *time.Time) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO scim_tokens (id, name, token_hash, token_prefix, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, name, tokenHash, tokenPrefix, createdBy, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateScimToken err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetActiveScimTokenByHash returns the live credential for a hash, or (nil, nil) if there is none.
//
// Runs on every SCIM request. "Live" means not revoked and not expired; a missing row and a revoked row
// are deliberately indistinguishable to the caller so there is no existence oracle.
func GetActiveScimTokenByHash(ctx context.Context, tokenHash string) (*ScimToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + scimTokenColumns + ` FROM scim_tokens
		WHERE token_hash=$1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())`
	t, err := scanScimToken(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetActiveScimTokenByHash err: %+v", err)
		return nil, err
	}
	return t, nil
}

// TouchScimTokenLastUsed records that a credential was used. Best-effort, called async.
//
// This is the only signal an operator has that a directory connection is alive. A SCIM integration that
// silently stopped syncing looks exactly like one with nothing to sync, and the difference matters:
// the second is fine and the first means joiners are not getting accounts.
func TouchScimTokenLastUsed(ctx context.Context, id uuid.UUID) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE scim_tokens SET last_used_at=NOW() WHERE id=$1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id); err != nil {
		helpers.LogErrorWithContext(ctx, "models/TouchScimTokenLastUsed err: %+v", err)
	}
}

// RevokeScimToken marks a credential revoked. Idempotent.
//
// NOT scoped to the creator, unlike api_tokens.RevokeToken. The credential belongs to the workspace, so
// any admin must be able to revoke it — requiring the original creator would mean a leaked credential
// cannot be pulled once that person has left, which is the same failure the table was designed to avoid.
func RevokeScimToken(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE scim_tokens SET revoked_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND revoked_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RevokeScimToken err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListScimTokens returns every credential, live first then newest. Secrets are never included.
func ListScimTokens(ctx context.Context) ([]*ScimToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + scimTokenColumns + ` FROM scim_tokens
		ORDER BY revoked_at IS NULL DESC, created_at DESC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListScimTokens err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*ScimToken
	for rows.Next() {
		t, scanErr := scanScimToken(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
