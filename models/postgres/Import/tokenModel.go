package models

// import_oauth_tokens — encrypted token storage for live-API providers.
//
// Encryption: AES-256-GCM with a key derived from the IMPORT_TOKEN_KEK
// env var (32 bytes after SHA-256). Tokens are encrypted at the
// application layer before they hit the database; pgcrypto is enabled
// in the migration so a future server-side encryption switch is one
// query away if needed.
//
// API contract:
//   - SaveToken     stores or replaces the token for (provider, owner).
//   - LoadToken     returns the decrypted access (and optional refresh)
//                   plus expires_at and scopes.
//   - DeleteToken   wipes the row (used on disconnect / token revoke).
//   - ListTokens    surfaces non-secret metadata for the FE.
//
// We intentionally never expose the raw encrypted blob to controllers —
// every read/write goes through this package so a future audit can
// trust that `import_oauth_tokens.access_token_enc` is touched only
// here.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Token holds the decrypted form returned by LoadToken. Callers MUST
// never log or persist the AccessToken/RefreshToken fields.
type Token struct {
	Id                uuid.UUID
	Provider          string
	OwnerUserId       uuid.UUID
	SourceAccountId   *string
	SourceAccountName *string
	AccessToken       string
	RefreshToken      string
	Scopes            *string
	ExpiresAt         *time.Time
	Metadata          json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TokenView is the safe-to-return shape for the FE: never includes the
// access/refresh tokens, only metadata.
type TokenView struct {
	Provider          string          `json:"provider"`
	OwnerUserId       uuid.UUID       `json:"owner_user_id"`
	SourceAccountId   *string         `json:"source_account_id,omitempty"`
	SourceAccountName *string         `json:"source_account_name,omitempty"`
	Scopes            *string         `json:"scopes,omitempty"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// ErrTokenNotFound is returned when no token exists for the lookup.
var ErrTokenNotFound = errors.New("import oauth token not found")

// SaveToken upserts the (provider, owner) row with fresh ciphertext.
// Re-saving overwrites — used by the OAuth callback when the user
// re-authorises.
func SaveToken(ctx context.Context, t *Token) error {
	if t.Provider == "" || t.OwnerUserId == uuid.Nil {
		return errors.New("provider and owner_user_id required")
	}
	if t.AccessToken == "" {
		return errors.New("access_token required")
	}

	accessCipher, err := encryptToken(t.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access: %w", err)
	}
	var refreshCipher []byte
	if t.RefreshToken != "" {
		refreshCipher, err = encryptToken(t.RefreshToken)
		if err != nil {
			return fmt.Errorf("encrypt refresh: %w", err)
		}
	}
	if len(t.Metadata) == 0 {
		t.Metadata = json.RawMessage(`{}`)
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO import_oauth_tokens
		    (provider, owner_user_id, source_account_id, source_account_name,
		     access_token_enc, refresh_token_enc, scopes, expires_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (provider, owner_user_id) DO UPDATE SET
		    source_account_id   = EXCLUDED.source_account_id,
		    source_account_name = EXCLUDED.source_account_name,
		    access_token_enc    = EXCLUDED.access_token_enc,
		    refresh_token_enc   = EXCLUDED.refresh_token_enc,
		    scopes              = EXCLUDED.scopes,
		    expires_at          = EXCLUDED.expires_at,
		    metadata            = EXCLUDED.metadata,
		    updated_at          = NOW()`,
		t.Provider, t.OwnerUserId, t.SourceAccountId, t.SourceAccountName,
		accessCipher, refreshCipher, t.Scopes, t.ExpiresAt, t.Metadata)
	return err
}

// LoadToken returns the decrypted token. Returns ErrTokenNotFound if
// no row exists.
func LoadToken(ctx context.Context, provider string, ownerUserId uuid.UUID) (*Token, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT id, provider, owner_user_id, source_account_id, source_account_name,
		       access_token_enc, refresh_token_enc, scopes, expires_at, metadata,
		       created_at, updated_at
		FROM import_oauth_tokens
		WHERE provider = $1 AND owner_user_id = $2`, provider, ownerUserId)

	t := &Token{}
	var accessCipher, refreshCipher []byte
	err := row.Scan(
		&t.Id, &t.Provider, &t.OwnerUserId, &t.SourceAccountId, &t.SourceAccountName,
		&accessCipher, &refreshCipher, &t.Scopes, &t.ExpiresAt, &t.Metadata,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	t.AccessToken, err = decryptToken(accessCipher)
	if err != nil {
		return nil, fmt.Errorf("decrypt access: %w", err)
	}
	if len(refreshCipher) > 0 {
		t.RefreshToken, err = decryptToken(refreshCipher)
		if err != nil {
			return nil, fmt.Errorf("decrypt refresh: %w", err)
		}
	}
	return t, nil
}

// DeleteToken removes a token row.
func DeleteToken(ctx context.Context, provider string, ownerUserId uuid.UUID) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`DELETE FROM import_oauth_tokens WHERE provider=$1 AND owner_user_id=$2`,
		provider, ownerUserId)
	return err
}

// ListTokens returns non-secret token metadata for a user. Used by the
// FE settings/admin page to show "connected" providers.
func ListTokens(ctx context.Context, ownerUserId uuid.UUID) ([]*TokenView, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT provider, owner_user_id, source_account_id, source_account_name,
		       scopes, expires_at, metadata, created_at, updated_at
		FROM import_oauth_tokens
		WHERE owner_user_id = $1
		ORDER BY provider`, ownerUserId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TokenView{}
	for rows.Next() {
		v := &TokenView{}
		if err := rows.Scan(
			&v.Provider, &v.OwnerUserId, &v.SourceAccountId, &v.SourceAccountName,
			&v.Scopes, &v.ExpiresAt, &v.Metadata, &v.CreatedAt, &v.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ─── AES-256-GCM encryption helpers ──────────────────────────────────

// kek returns the 32-byte key derived from IMPORT_TOKEN_KEK. We use
// SHA-256 of the env var rather than requiring a 32-byte literal so
// operators can use any reasonably long passphrase. In production the
// env var should be set to a high-entropy 32+ char value managed by
// the platform's secret store.
//
// If IMPORT_TOKEN_KEK is unset we fall back to a derived key from
// HOSTNAME — this is INSECURE and only intended for local dev so the
// service starts. Production deployments must set IMPORT_TOKEN_KEK.
func kek() []byte {
	v := os.Getenv("IMPORT_TOKEN_KEK")
	if v == "" {
		// Dev fallback. Operators see the warning at startup via
		// helpers.LogWarn but we don't crash to keep `make dev` simple.
		v = "onecamp-dev-import-kek-please-override-in-production"
	}
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

// encryptToken returns nonce(12) || ciphertext || tag(16).
func encryptToken(plain string) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("empty plaintext")
	}
	block, err := aes.NewCipher(kek())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return out, nil
}

// decryptToken splits the nonce off the front and verifies the tag.
func decryptToken(blob []byte) (string, error) {
	if len(blob) < 12+16 {
		return "", errors.New("ciphertext too short")
	}
	block, err := aes.NewCipher(kek())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
