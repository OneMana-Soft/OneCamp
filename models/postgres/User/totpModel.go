package models

// Storage for TOTP enrolment and recovery codes (migration 141).
//
// The shape of the state matters more than the queries, so it is stated once here:
//
//	secret NULL                      -> not enrolled. No challenge at login.
//	secret set, confirmed_at NULL    -> enrolment STARTED and abandoned. No challenge at login.
//	secret set, confirmed_at set     -> enrolled. Login challenges.
//
// The middle state exists so that scanning a QR code and closing the tab cannot lock anyone out. A
// design where generating the secret enrols the user has a failure mode that is both easy to hit and
// impossible for the user to escape, and support cannot distinguish it from a lost phone.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ErrTOTPKEKMissing means TOTP_KEK is not configured, so a secret cannot be sealed or opened.
//
// A SENTINEL, and deliberately NOT a fallback to a built-in key. AI_CONFIG_KEK has a dev fallback
// because a local checkout needs to start without ceremony and the cost is a provider key encrypted
// weakly on a developer's laptop. The cost here is different in kind: a second factor sealed under a
// key that ships in the source protects nothing, while looking to the user, the admin and any auditor
// exactly like protection. Refusing to enrol is a visible dead end; a decorative second factor is not.
var ErrTOTPKEKMissing = errors.New("TOTP_KEK is not set, so two-factor authentication cannot be " +
	"enabled. Generate one with `openssl rand -base64 48` and set it in the environment")

// ErrTOTPSecretUnreadable means a secret is stored but will not decrypt.
//
// Mirrors AI.ErrProviderKeyUnreadable and AIMCP.ErrAuthSecretUnreadable, for the same reason and with
// the same cause: TOTP_KEK changed and everything sealed under the old value is now undecryptable. The
// login path must treat this as "this account cannot complete a challenge" and say so, rather than as a
// wrong code — otherwise a user types correct codes forever and is told each one is wrong.
var ErrTOTPSecretUnreadable = errors.New("this account's two-factor secret can no longer be decrypted " +
	"(usually because TOTP_KEK changed); an administrator must reset two-factor authentication for it")

// totpKEK derives the 32-byte key from TOTP_KEK.
//
// SEPARATE FROM AI_CONFIG_KEK on purpose, and the reason is operational rather than theoretical: that
// key is rotated when a provider credential leaks, and this deployment has already replaced it once.
// Sharing it would mean an unrelated credential rotation locks every enrolled user out of their own
// account.
func totpKEK() ([]byte, error) {
	v := strings.TrimSpace(os.Getenv("TOTP_KEK"))
	if v == "" {
		return nil, ErrTOTPKEKMissing
	}
	sum := sha256.Sum256([]byte(v))
	return sum[:], nil
}

// sealTOTPSecret returns nonce(12) || ciphertext || tag(16), matching the scheme used for AI provider
// keys and OAuth import tokens so there is one envelope format to reason about.
func sealTOTPSecret(secret string) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("empty totp secret")
	}
	key, err := totpKEK()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
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
	return gcm.Seal(nonce, nonce, []byte(secret), nil), nil
}

// openTOTPSecret reverses sealTOTPSecret, and distinguishes "no key" from "wrong key".
func openTOTPSecret(blob []byte) (string, error) {
	if len(blob) < 12+16 {
		return "", ErrTOTPSecretUnreadable
	}
	key, err := totpKEK()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// A GCM tag failure here means the key changed, not that the row is corrupt. Reported as the
		// operator condition it is, so the login path can say something true.
		return "", ErrTOTPSecretUnreadable
	}
	return string(plain), nil
}

// HashRecoveryCode returns the stored form of a recovery code.
//
// SHA-256 rather than bcrypt, which is a considered departure from how passwords are stored here. See
// migration 141 for the full reasoning; the short version is that a recovery code is generated rather
// than chosen, so stretching defends against a brute force that is already infeasible, while costing
// one expensive comparison per stored code at verification time — an unauthenticated way to burn a
// second of CPU per attempt.
//
// Normalised first so the code a user types matches the code they were shown: people reproduce these
// from paper, in the case and grouping they happen to remember.
func HashRecoveryCode(code string) string {
	normalised := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

// NewRecoveryCode returns one display-form recovery code.
//
// Base32 without padding, uppercase, in two groups of five, from 50 bits of entropy. Base32 rather than
// hex or base64 because these are read off paper and typed: it excludes the characters people confuse
// (no 0/O, no 1/I/l) and is case-insensitive, so the transcription errors that produce a support ticket
// are largely designed out.
func NewRecoveryCode() (string, error) {
	raw := make([]byte, 7) // 56 bits, 8 base32 characters after truncation to 10
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("recovery code: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	if len(encoded) > 10 {
		encoded = encoded[:10]
	}
	return encoded[:5] + "-" + encoded[5:], nil
}

// TOTPStatus is what the settings screen needs to render.
type TOTPStatus struct {
	// Enrolled is true only for a CONFIRMED enrolment, i.e. the state that challenges at login.
	Enrolled bool
	// PendingEnrolment is true when a secret exists that was never confirmed.
	PendingEnrolment bool
	// UnusedRecoveryCodes is how many codes remain. Shown so a user can tell they are running low
	// BEFORE the one occasion they need them.
	UnusedRecoveryCodes int
}

// GetTOTPStatus reports enrolment state for a user.
func GetTOTPStatus(ctx context.Context, userID uuid.UUID) (TOTPStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var status TOTPStatus
	var secret []byte
	var confirmedAt sql.NullTime

	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_confirmed_at,
		        (SELECT COUNT(*) FROM user_recovery_codes
		          WHERE user_id = users.id AND used_at IS NULL)
		   FROM users WHERE id = $1`,
		userID,
	).Scan(&secret, &confirmedAt, &status.UnusedRecoveryCodes)
	if err != nil {
		return TOTPStatus{}, err
	}

	status.Enrolled = len(secret) > 0 && confirmedAt.Valid
	status.PendingEnrolment = len(secret) > 0 && !confirmedAt.Valid
	return status, nil
}

// BeginTOTPEnrollment stores an unconfirmed secret, replacing any previous attempt.
//
// Explicitly clears totp_confirmed_at and resets totp_last_step. Re-enrolling with a new device must
// not inherit the spent-step ceiling of the old one: a fresh secret produces codes from the same clock,
// so a stale high-water mark would refuse the first code the user reads off their new phone.
func BeginTOTPEnrollment(ctx context.Context, userID uuid.UUID, secret string) error {
	sealed, err := sealTOTPSecret(secret)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE users
		    SET totp_secret_enc = $2, totp_confirmed_at = NULL, totp_last_step = 0, updated_at = NOW()
		  WHERE id = $1`,
		userID, sealed,
	)
	return err
}

// ConfirmTOTPEnrollment marks an enrolment live and installs its recovery codes, atomically.
//
// ONE TRANSACTION because the halves are worthless apart. Confirming without codes enrols a user with
// no way back from a lost phone; writing codes without confirming leaves codes for an enrolment that
// does not challenge. A failure between the two, on a path a user runs exactly once, is not something to
// discover later.
//
// step is the time-step the confirming code matched, recorded so that same code cannot immediately be
// replayed as a login.
func ConfirmTOTPEnrollment(ctx context.Context, userID uuid.UUID, step int64, codeHashes []string) error {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE users
		    SET totp_confirmed_at = NOW(), totp_last_step = $2, updated_at = NOW()
		  WHERE id = $1 AND totp_secret_enc IS NOT NULL`,
		userID, step,
	)
	if err != nil {
		return err
	}
	// Guards against confirming an enrolment that was never begun, which would otherwise silently
	// succeed and leave a user "enrolled" with no secret to challenge against.
	if affected, aerr := res.RowsAffected(); aerr == nil && affected == 0 {
		return errors.New("no pending two-factor enrolment for this user")
	}

	// Replace rather than append: issuing a new set must invalidate the old one, or a code from a
	// previous enrolment stays redeemable against a device the user no longer has.
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, hash := range codeHashes {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, hash,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// TOTPLoginSecret is the state the login path needs to evaluate a challenge.
type TOTPLoginSecret struct {
	Secret    string
	Confirmed bool
	LastStep  int64
}

// LoadTOTPForLogin returns the decrypted secret and replay ceiling for a user.
//
// Returns ErrTOTPSecretUnreadable when a secret exists but the KEK cannot open it, so the caller can
// report the operator condition instead of rejecting correct codes forever.
func LoadTOTPForLogin(ctx context.Context, userID uuid.UUID) (TOTPLoginSecret, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var sealed []byte
	var confirmedAt sql.NullTime
	var lastStep int64

	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_confirmed_at, totp_last_step FROM users WHERE id = $1`,
		userID,
	).Scan(&sealed, &confirmedAt, &lastStep)
	if err != nil {
		return TOTPLoginSecret{}, err
	}
	if len(sealed) == 0 {
		return TOTPLoginSecret{}, nil
	}

	secret, err := openTOTPSecret(sealed)
	if err != nil {
		return TOTPLoginSecret{}, err
	}
	return TOTPLoginSecret{Secret: secret, Confirmed: confirmedAt.Valid, LastStep: lastStep}, nil
}

// RecordTOTPStep advances the replay ceiling after a successful challenge.
//
// The comparison is in the WHERE clause rather than in Go so two concurrent logins cannot both write
// the same step: the second UPDATE matches no row and the ceiling only ever moves forward. Doing this
// check by reading and then writing would leave exactly the race the ceiling exists to prevent.
func RecordTOTPStep(ctx context.Context, userID uuid.UUID, step int64) error {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE users SET totp_last_step = $2 WHERE id = $1 AND totp_last_step < $2`,
		userID, step,
	)
	return err
}

// RedeemRecoveryCode spends a code if it is valid and unused, and reports whether it was.
//
// The spend is a single conditional UPDATE, so two simultaneous attempts with the same code cannot both
// succeed: `used_at IS NULL` is evaluated by the database, and only one statement can match the row.
// Reading the row and then updating it would let a code be redeemed twice under a race — the same class
// of bug as the replay this whole file guards against.
func RedeemRecoveryCode(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE user_recovery_codes SET used_at = NOW()
		  WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`,
		userID, HashRecoveryCode(code),
	)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// DisableTOTP removes a user's second factor and every recovery code with it.
//
// Codes are deleted rather than left behind: they authenticate against an enrolment that no longer
// exists, and leaving redeemable credentials attached to a disabled factor is the kind of residue that
// turns into an incident when 2FA is re-enabled later.
func DisableTOTP(ctx context.Context, userID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx,
		`UPDATE users
		    SET totp_secret_enc = NULL, totp_confirmed_at = NULL, totp_last_step = 0, updated_at = NOW()
		  WHERE id = $1`, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM user_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
