package business

// Two-factor authentication logic: enrolment, the login challenge, and recovery.
//
// THE SHAPE OF THE LOGIN FLOW, because it is the part with security consequences.
//
// A password alone must never mint a session for an enrolled user. So EmailLogin, on a correct
// password, stops and returns a CHALLENGE instead of cookies:
//
//	POST /auth/login        {email, password}          -> 200 {status:"totp_required", challenge:"..."}
//	POST /auth/login/totp   {challenge, code}          -> 200 + auth cookies
//
// The challenge is a short-lived signed token that proves the password step happened, and nothing else.
// It is NOT an auth token: it carries a purpose claim, it is checked for that claim, and no middleware
// accepts it. Getting this wrong in the obvious way — issuing the real session and asking for the code
// afterwards — means the second factor is advisory, because the first response already contained
// everything an attacker needed.
//
// WHY A SIGNED TOKEN RATHER THAN SERVER-SIDE STATE. Redis is available and would give single-use
// semantics for free. It was not used, because the property that actually matters is that a CODE cannot
// be replayed, and that is enforced in the database against totp_last_step regardless of how the
// challenge is carried. Putting the challenge in Redis would add an availability dependency to the login
// path — and the honest policy for that outage would be to fail closed, i.e. a cache outage becomes a
// login outage for exactly the users who took security seriously enough to enrol.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// totpChallengePurpose distinguishes this token from a session token.
	//
	// Checked on the way in, not merely set on the way out. A signed token with the right key but the
	// wrong purpose is exactly the confusion this prevents: without the check, an auth cookie could be
	// presented as a challenge, or worse, a challenge accepted as a session.
	totpChallengePurpose = "totp_challenge"

	// totpChallengeTTL is how long the password step stays good for.
	//
	// Five minutes: long enough to find a phone, unlock it and read a code, short enough that a
	// challenge captured from a browser is worthless by the time it is used. The window is not a
	// replay risk on its own — a code is still required, and codes are single-use.
	totpChallengeTTL = 5 * time.Minute

	// recoveryCodeCount is how many codes are issued at enrolment.
	//
	// Ten is the conventional number and the reasoning is about behaviour: enough that a user who
	// burns one or two does not immediately need to regenerate, few enough to fit on the piece of
	// paper or in the password-manager note where they will actually end up.
	recoveryCodeCount = 10
)

// ErrTOTPChallengeInvalid means the challenge is absent, malformed, expired, or not a challenge.
//
// One error for all of them, matching the house pattern for credentials elsewhere in this codebase: a
// caller that can distinguish "expired" from "forged" from "that was a session token" learns about
// state it was never granted, and the distinction is useless to a legitimate client, which either has a
// fresh challenge or needs to start again.
var ErrTOTPChallengeInvalid = errors.New("this sign-in attempt has expired. Enter your password again")

// ErrTOTPCodeInvalid means the code did not verify.
//
// Deliberately identical for a wrong code, a reused code, and a code from outside the clock window.
// "That code was correct but already used" tells an attacker holding an observed code that they have the
// right secret and merely need a fresh one.
var ErrTOTPCodeInvalid = errors.New("that code is not valid. Check your authenticator app and try again")

// ErrTOTPNotEnrolled means the account has no confirmed second factor.
var ErrTOTPNotEnrolled = errors.New("two-factor authentication is not enabled for this account")

// totpIssuer is the name shown in the user's authenticator app beside their account.
func totpIssuer() string {
	if name := strings.TrimSpace(os.Getenv("ORG_NAME")); name != "" {
		return name
	}
	return "OneCamp"
}

// IssueTOTPChallenge mints the token that carries a completed password step.
func IssueTOTPChallenge(userID uuid.UUID) (string, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		// The same key that signs sessions. Its absence is a misconfiguration that must not degrade
		// into an unsigned or predictable challenge.
		return "", errors.New("JWT_SECRET is not configured")
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     userID.String(),
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(totpChallengeTTL).Unix(),
		"iat":     time.Now().Unix(),
	})
	return token.SignedString([]byte(secret))
}

// ParseTOTPChallenge validates a challenge and returns the user it was issued for.
func ParseTOTPChallenge(raw string) (uuid.UUID, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return uuid.UUID{}, errors.New("JWT_SECRET is not configured")
	}

	parsed, err := jwt.Parse(strings.TrimSpace(raw), func(t *jwt.Token) (interface{}, error) {
		// The algorithm is pinned. Accepting whatever the token declares is how "alg: none" and
		// HMAC-for-RSA confusions get in, and this token gates a login.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		return uuid.UUID{}, ErrTOTPChallengeInvalid
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return uuid.UUID{}, ErrTOTPChallengeInvalid
	}
	// THE PURPOSE CHECK. Without it a session token, signed with this same key, would be accepted here.
	if purpose, _ := claims["purpose"].(string); purpose != totpChallengePurpose {
		return uuid.UUID{}, ErrTOTPChallengeInvalid
	}
	subject, _ := claims["sub"].(string)
	userID, perr := uuid.Parse(subject)
	if perr != nil {
		return uuid.UUID{}, ErrTOTPChallengeInvalid
	}
	return userID, nil
}

// TOTPEnrollment is what the user needs to add the account to an authenticator app.
type TOTPEnrollment struct {
	// Secret in base32, shown so a user whose camera cannot scan can type it in.
	Secret string
	// URI is the otpauth:// string the frontend renders as a QR code.
	URI string
}

// BeginTOTPEnrollment generates a secret and stores it UNCONFIRMED.
//
// Nothing about the account changes until the user proves they can produce a code. Enrolling on
// generation would lock out anyone who scans a QR code and closes the tab, and that user cannot tell
// their situation apart from a broken second factor.
func BeginTOTPEnrollment(ctx context.Context, userID uuid.UUID, accountName string) (TOTPEnrollment, error) {
	secret, err := helpers.NewTOTPSecret()
	if err != nil {
		return TOTPEnrollment{}, err
	}
	if err := userModels.BeginTOTPEnrollment(ctx, userID, secret); err != nil {
		// Includes ErrTOTPKEKMissing, which the controller surfaces as an operator condition rather
		// than a user error: there is nothing the person clicking "enable" can do about it.
		return TOTPEnrollment{}, err
	}
	return TOTPEnrollment{
		Secret: secret,
		URI:    helpers.TOTPProvisioningURI(totpIssuer(), accountName, secret),
	}, nil
}

// ConfirmTOTPEnrollment verifies the first code and returns the recovery codes, once.
//
// The codes are returned in PLAINTEXT here and never again — only their hashes are stored. This is the
// only moment they can be shown, which is why the frontend must present them as something to save
// rather than as a confirmation to dismiss.
func ConfirmTOTPEnrollment(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	stored, err := userModels.LoadTOTPForLogin(ctx, userID)
	if err != nil {
		return nil, err
	}
	if stored.Secret == "" {
		return nil, errors.New("start two-factor setup before confirming it")
	}
	if stored.Confirmed {
		return nil, errors.New("two-factor authentication is already enabled for this account")
	}

	step, ok := helpers.ValidateTOTP(stored.Secret, code, time.Now(), stored.LastStep)
	if !ok {
		return nil, ErrTOTPCodeInvalid
	}

	plain := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		rc, rerr := userModels.NewRecoveryCode()
		if rerr != nil {
			return nil, rerr
		}
		plain = append(plain, rc)
		hashes = append(hashes, userModels.HashRecoveryCode(rc))
	}

	if err := userModels.ConfirmTOTPEnrollment(ctx, userID, step, hashes); err != nil {
		return nil, err
	}
	return plain, nil
}

// TOTPRequired reports whether a user must answer a challenge to sign in.
func TOTPRequired(ctx context.Context, userID uuid.UUID) (bool, error) {
	status, err := userModels.GetTOTPStatus(ctx, userID)
	if err != nil {
		return false, err
	}
	return status.Enrolled, nil
}

// CompleteTOTPChallenge accepts either an authenticator code or a recovery code.
//
// TOTP IS TRIED FIRST, and the order is deliberate: a six-digit numeric string cannot be one of this
// system's recovery codes (base32, grouped, ten characters plus a hyphen), so trying TOTP first cannot
// accidentally burn a recovery code on a mistyped digit.
//
// A used-up recovery code is spent whether or not the login then succeeds, which is correct — it has
// been transmitted and observed.
func CompleteTOTPChallenge(ctx context.Context, userID uuid.UUID, code string) error {
	stored, err := userModels.LoadTOTPForLogin(ctx, userID)
	if err != nil {
		// ErrTOTPSecretUnreadable arrives here when TOTP_KEK changed. Passed through so the caller can
		// say so, instead of telling a user with a correct code that it is wrong, forever.
		return err
	}
	if !stored.Confirmed {
		return ErrTOTPNotEnrolled
	}

	if step, ok := helpers.ValidateTOTP(stored.Secret, code, time.Now(), stored.LastStep); ok {
		// Recorded BEFORE the session is issued. If this write fails the login fails, because the
		// alternative is a session minted against a code that can still be replayed.
		return userModels.RecordTOTPStep(ctx, userID, step)
	}

	redeemed, err := userModels.RedeemRecoveryCode(ctx, userID, code)
	if err != nil {
		return err
	}
	if !redeemed {
		return ErrTOTPCodeInvalid
	}
	return nil
}

// DisableTOTP turns off a user's second factor, and REQUIRES a valid code to do it.
//
// Demanding a code is the point. Without it, anyone holding a hijacked session — the exact thing the
// second factor exists to survive — could remove it in one request, and the protection would be
// decorative against the attack it is meant to stop.
//
// A recovery code is accepted as well as an authenticator code, because a user whose phone is gone still
// has to be able to turn this off.
func DisableTOTP(ctx context.Context, userID uuid.UUID, code string) error {
	if err := CompleteTOTPChallenge(ctx, userID, code); err != nil {
		return err
	}
	return userModels.DisableTOTP(ctx, userID)
}

// TOTPStatusFor reports enrolment state for the settings screen.
func TOTPStatusFor(ctx context.Context, userID uuid.UUID) (userModels.TOTPStatus, error) {
	return userModels.GetTOTPStatus(ctx, userID)
}

// ErrTOTPSelfResetRefused means an admin aimed the reset at their own account.
var ErrTOTPSelfResetRefused = errors.New(
	"an administrator cannot reset their own second factor here; ask another administrator. " +
		"Use a recovery code if you still have one")

// AdminResetTOTP clears a user's second factor WITHOUT a code from them.
//
// WHY THIS HAS TO EXIST. DisableTOTP above demands a valid code, deliberately, so a hijacked session
// cannot strip the protection it is meant to survive. That leaves two situations with no way out at all:
// a user who has lost their phone AND their recovery codes, and — much worse because it is collective —
// every enrolled user at once if TOTP_KEK is ever replaced, since their stored secrets stop decrypting
// and no code they can produce will ever verify again. Without an administrative reset the only remedy
// in either case is editing the database by hand.
//
// WHY IT REFUSES SELF-TARGETING. Aiming it at your own account would be a plain bypass of the code
// requirement on DisableTOTP: an attacker holding a stolen ADMIN session could strip their own second
// factor in one request, which is exactly the attack that check exists to stop. So a reset needs a
// second person, which is the right answer for a privileged account. An operator who is the only admin
// and has lost everything still has recovery codes, and failing that direct database access — a
// self-hoster is never truly locked out, and the alternative is a session-theft bypass shipped as a
// convenience.
//
// Reports whether a factor was actually removed, so the audit record and the response can distinguish a
// real reset from a no-op on an account that had none. Recording "reset the second factor" for a user
// who never had one would put a security event in the log that did not happen.
func AdminResetTOTP(ctx context.Context, actingAdminID uuid.UUID, targetUserID uuid.UUID) (wasEnrolled bool, err error) {
	if actingAdminID == targetUserID {
		return false, ErrTOTPSelfResetRefused
	}

	// Read the state BEFORE clearing it. Afterwards there is nothing left to tell whether this account
	// had a factor, and the audit summary depends on knowing.
	status, err := userModels.GetTOTPStatus(ctx, targetUserID)
	if err != nil {
		return false, err
	}

	// Nothing enrolled and nothing pending: return without writing. Idempotent rather than an error,
	// because a second click is not a failure — but it must not claim to have removed anything.
	if !status.Enrolled && !status.PendingEnrolment {
		return false, nil
	}

	if err := userModels.DisableTOTP(ctx, targetUserID); err != nil {
		return false, err
	}

	// Enrolled specifically, not "enrolled or pending". An abandoned enrolment was never enforcing
	// anything, so clearing it is housekeeping and should not read in the audit trail as a protection
	// having been removed from someone.
	return status.Enrolled, nil
}
