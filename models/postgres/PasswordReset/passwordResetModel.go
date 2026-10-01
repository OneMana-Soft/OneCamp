package models

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// HashResetToken returns the stored form of a reset token.
//
// THE TOKEN IS A BEARER CREDENTIAL. Anyone holding it can set the password on the
// account it belongs to, without knowing the old one — so a plaintext column is a
// list of working skeleton keys for every reset currently in flight. Read once (a
// leaked backup, a SQL injection, anybody with SELECT on the replica) and they can
// be replayed until they expire. Hashed, that read yields nothing usable.
//
// SHA-256 RATHER THAN BCRYPT, the same considered departure the recovery codes make
// (see migration 141): this token is generated, not chosen, and carries 256 bits of
// entropy, so stretching defends against a brute force that is already infeasible
// while making every validation expensive.
//
// NOT NORMALISED, unlike HashRecoveryCode. That one upper-cases and strips spaces
// because people copy recovery codes off paper in whatever grouping they remember.
// This one arrives in a URL, machine to machine, and folding case would throw away
// distinctions the generator relies on.
func HashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type PasswordResetToken struct {
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id"`
	// Token is the STORED form — a hash. The value that went out in the email cannot
	// be recovered from here, which is the point. Never return it to a caller.
	Token     string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	CreatedAt time.Time `json:"created_at"`
}

func CreateResetToken(userID uuid.UUID, token string, expiresAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		INSERT INTO password_reset_tokens (user_id, token, expires_at)
		VALUES ($1, $2, $3)
	`

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, userID, HashResetToken(token), expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateResetToken Failed to create reset token err: %+v",
			err)
		return
	}

	return
}

func GetValidResetToken(token string) (resetToken *PasswordResetToken, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		SELECT id, user_id, token, expires_at, used, created_at
		FROM password_reset_tokens
		WHERE token = $1 AND used = false AND expires_at > NOW()
	`

	var rt PasswordResetToken
	var createdAt sql.NullTime

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, HashResetToken(token)).Scan(
		&rt.ID,
		&rt.UserID,
		&rt.Token,
		&rt.ExpiresAt,
		&rt.Used,
		&createdAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetValidResetToken Failed to get reset token err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		rt.CreatedAt = createdAt.Time
	}

	return &rt, nil
}

func MarkTokenUsed(tokenID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `UPDATE password_reset_tokens SET used = true WHERE id = $1`

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, tokenID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/MarkTokenUsed Failed to mark token as used err: %+v",
			err)
		return
	}

	return
}

// InvalidateAllTokensForUser marks all unused tokens for a user as used (e.g., after password reset)
func InvalidateAllTokensForUser(userID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `UPDATE password_reset_tokens SET used = true WHERE user_id = $1 AND used = false`

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/InvalidateAllTokensForUser Failed to invalidate tokens err: %+v",
			err)
		return
	}

	return
}
