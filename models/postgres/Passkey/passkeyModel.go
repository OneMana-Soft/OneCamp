// Package models (Passkey) stores people's passkeys (migration 182).
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Passkey is one of a person's passkeys. Credential is the WebAuthn library's
// record of it, kept as JSON so new fields need no migration.
type Passkey struct {
	Id         uuid.UUID       `json:"id"`
	UserID     uuid.UUID       `json:"-"`
	Credential json.RawMessage `json:"-"`
	Name       string          `json:"name"`
	CreatedAt  time.Time       `json:"created_at"`
	LastUsedAt *time.Time      `json:"last_used_at"`
}

// Owner is who a passkey signs in as, and whether they still may.
type Owner struct {
	UserID       uuid.UUID
	Email        string
	Name         string
	IsSSOManaged bool
	IsExternal   bool
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const columns = `id, user_id, credential, name, created_at, last_used_at`

func scan(r interface{ Scan(...any) error }) (*Passkey, error) {
	var p Passkey
	var used sql.NullTime
	if err := r.Scan(&p.Id, &p.UserID, &p.Credential, &p.Name, &p.CreatedAt, &used); err != nil {
		return nil, err
	}
	if used.Valid {
		p.LastUsedAt = &used.Time
	}
	return &p, nil
}

// List is a person's passkeys, newest first.
func List(userID uuid.UUID) ([]Passkey, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+columns+` FROM user_passkeys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ByCredentialID is the passkey with this credential id, or nil.
func ByCredentialID(credentialID []byte) (*Passkey, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	p, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+columns+` FROM user_passkeys WHERE credential_id = $1`, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// OwnerOf is a person who can still sign in: not deleted. Nil otherwise.
func OwnerOf(userID uuid.UUID) (*Owner, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var o Owner
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT id, email_id, COALESCE(NULLIF(display_name, ''), username, email_id), COALESCE(is_sso_managed, false), COALESCE(is_external, false)
		  FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).
		Scan(&o.UserID, &o.Email, &o.Name, &o.IsSSOManaged, &o.IsExternal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// MaxPerPerson bounds how many passkeys one person keeps.
const MaxPerPerson = 20

// Create stores a new passkey.
func Create(userID uuid.UUID, credentialID []byte, credential json.RawMessage, name string) (*Passkey, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO user_passkeys (user_id, credential_id, credential, name) VALUES ($1, $2, $3, $4)
		RETURNING `+columns, userID, credentialID, []byte(credential), name))
}

// Used records a sign-in: the credential's new sign count and flags.
func Used(credentialID []byte, credential json.RawMessage, at time.Time) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE user_passkeys SET credential = $2, last_used_at = $3 WHERE credential_id = $1`, credentialID, []byte(credential), at)
	return err
}

// Rename names one of the person's passkeys; false when it wasn't theirs.
func Rename(id, userID uuid.UUID, name string) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE user_passkeys SET name = $3 WHERE id = $1 AND user_id = $2`, id, userID, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Delete removes one of the person's passkeys; false when it wasn't theirs.
func Delete(id, userID uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM user_passkeys WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
