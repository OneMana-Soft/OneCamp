// Package models (ApiToken) is the Postgres data-access layer for scoped API
// tokens (migration 92). Only the SHA-256 hash of a token is stored; lookups
// are by hash. The business layer generates tokens, hashes them, and checks
// scopes.
package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ApiToken mirrors a row of api_tokens. The secret itself is never stored.
type ApiToken struct {
	Id          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      string     `json:"scopes"` // raw JSON array
	CreatedBy   uuid.UUID  `json:"created_by"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// AgentId optionally binds this credential to an AGENT IDENTITY (migration 138).
	//
	// An agent is an identity; a token is a credential for it. Separate rows because
	// the relationship is one-to-many and the lifecycles differ — a credential is
	// rotated, an identity persists. NULL means a plain integration credential,
	// which is what every pre-existing token is and stays.
	//
	// When set, the agent is the actor in audit records and ai_agents.is_active
	// becomes an independent kill switch: deactivating the agent stops every
	// credential bound to it, without hunting for tokens to revoke.
	AgentId *uuid.UUID `json:"agent_id,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const tokenColumns = `id, name, token_prefix, scopes, created_by, last_used_at, expires_at, revoked_at, created_at, updated_at, agent_id`

func scanToken(s scanner) (*ApiToken, error) {
	var t ApiToken
	var lastUsedAt, expiresAt, revokedAt sql.NullTime
	var agentID uuid.NullUUID
	if err := s.Scan(&t.Id, &t.Name, &t.TokenPrefix, &t.Scopes, &t.CreatedBy,
		&lastUsedAt, &expiresAt, &revokedAt, &t.CreatedAt, &t.UpdatedAt, &agentID); err != nil {
		return nil, err
	}
	if agentID.Valid {
		t.AgentId = &agentID.UUID
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
	if strings.TrimSpace(t.Scopes) == "" {
		t.Scopes = "[]"
	}
	return &t, nil
}

// CreateToken inserts a new token row (hash + prefix + scopes) and returns its id.
//
// agentID binds the credential to an agent identity (migration 138), or is nil for a
// plain integration credential. Set only at creation, never afterwards: a credential
// that could change which identity it acts as would make every audit row before the
// change mean something different, and the audit trail is the point.
func CreateToken(ctx context.Context, name, tokenHash, tokenPrefix, scopesJSON string, createdBy uuid.UUID, expiresAt *time.Time, agentID *uuid.UUID) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(scopesJSON) == "" {
		scopesJSON = "[]"
	}
	id := uuid.New()
	const q = `INSERT INTO api_tokens (id, name, token_hash, token_prefix, scopes, created_by, expires_at, agent_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, name, tokenHash, tokenPrefix, scopesJSON, createdBy, expiresAt, agentID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateToken err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetActiveByHash returns the active (non-revoked, non-expired) token for a
// hash, or (nil, nil) if none. Used by the auth middleware on every request.
func GetActiveByHash(ctx context.Context, tokenHash string) (*ApiToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + tokenColumns + ` FROM api_tokens
		WHERE token_hash=$1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())`
	t, err := scanToken(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetActiveByHash err: %+v", err)
		return nil, err
	}
	return t, nil
}

// TouchLastUsed updates a token's last_used_at (best-effort; called async).
func TouchLastUsed(ctx context.Context, id uuid.UUID) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE api_tokens SET last_used_at=NOW() WHERE id=$1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id); err != nil {
		helpers.LogErrorWithContext(ctx, "models/TouchLastUsed err: %+v", err)
	}
}

// RevokeToken marks a token revoked for the owner (idempotent).
func RevokeToken(ctx context.Context, id, createdBy uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE api_tokens SET revoked_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND created_by=$2 AND revoked_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RevokeToken err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListByUser returns a user's tokens (active first, newest first). Secrets are
// never included.
func ListByUser(ctx context.Context, createdBy uuid.UUID) ([]*ApiToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + tokenColumns + ` FROM api_tokens
		WHERE created_by=$1 ORDER BY revoked_at IS NULL DESC, created_at DESC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListByUser err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*ApiToken
	for rows.Next() {
		t, scanErr := scanToken(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListActive returns every credential that can still be used, across the
// workspace, newest first. For the admin inventory, which has to answer "what
// can act here" rather than "what did I make".
func ListActive(ctx context.Context) ([]*ApiToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + tokenColumns + ` FROM api_tokens
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at DESC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListActive err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*ApiToken
	for rows.Next() {
		t, scanErr := scanToken(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAny revokes a credential whoever made it. Only an admin path may call
// this; RevokeToken is the owner's. Returns sql.ErrNoRows when there was
// nothing live to revoke.
func RevokeAny(ctx context.Context, id uuid.UUID) (*ApiToken, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE api_tokens SET revoked_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND revoked_at IS NULL RETURNING ` + tokenColumns
	t, err := scanToken(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			helpers.LogErrorWithContext(ctx, "models/RevokeAny err: %+v", err)
		}
		return nil, err
	}
	return t, nil
}

// RotateSecret replaces a live credential's secret and expiry in place.
// Returns sql.ErrNoRows when the credential is revoked or gone, which is what
// ends an OAuth connection an admin has revoked from the inventory.
func RotateSecret(ctx context.Context, id uuid.UUID, tokenHash, tokenPrefix string, expiresAt time.Time) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE api_tokens SET token_hash=$2, token_prefix=$3, expires_at=$4, updated_at=NOW()
		WHERE id=$1 AND revoked_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, tokenHash, tokenPrefix, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RotateSecret err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
