package business

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// withSecret sets JWT_SECRET for one test and restores it after.
func withSecret(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-jwt-secret-with-enough-length-1234567890")
}

// A challenge round-trips to the user it was issued for, and how they passed the first step: an email
// password or the directory's, each recorded as such once the code is in.
func TestTOTPChallengeRoundTrips(t *testing.T) {
	withSecret(t)
	userID := uuid.New()

	for _, method := range []string{userModels.AuthMethodEmail, userModels.AuthMethodLDAP} {
		challenge, err := IssueTOTPChallenge(userID, method)
		if err != nil {
			t.Fatalf("issue (%s): %v", method, err)
		}
		got, gotMethod, err := ParseTOTPChallenge(challenge)
		if err != nil {
			t.Fatalf("parse (%s): %v", method, err)
		}
		if got != userID || gotMethod != method {
			t.Errorf("round-tripped to %s by %q, want %s by %q", got, gotMethod, userID, method)
		}
	}

	// Single sign-on and passkeys hand out no challenge.
	if _, err := IssueTOTPChallenge(userID, userModels.AuthMethodSAML); err == nil {
		t.Error("issued a challenge for a SAML sign-in")
	}
}

// A challenge minted before it said how the first step was passed came from the only sign-in that
// handed one out then, a password; one saying anything a challenge can't is refused.
func TestTOTPChallengeMethodClaim(t *testing.T) {
	withSecret(t)
	userID := uuid.New()
	sign := func(claims jwt.MapClaims) string {
		t.Helper()
		claims["sub"] = userID.String()
		claims["purpose"] = totpChallengePurpose
		claims["exp"] = time.Now().Add(time.Minute).Unix()
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
			SignedString(challengeKey("test-jwt-secret-with-enough-length-1234567890"))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed
	}

	if _, method, err := ParseTOTPChallenge(sign(jwt.MapClaims{})); err != nil || method != userModels.AuthMethodEmail {
		t.Errorf("an older challenge: method %q, err %v; want email", method, err)
	}
	if _, _, err := ParseTOTPChallenge(sign(jwt.MapClaims{"method": userModels.AuthMethodSAML})); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("a challenge claiming a SAML sign-in: %v, want ErrTOTPChallengeInvalid", err)
	}
}

// A SESSION TOKEN MUST NOT BE ACCEPTED AS A CHALLENGE.
//
// The single most important property in this file. Session tokens are signed with the SAME JWT_SECRET,
// so signature validity proves nothing about what a token is for. Without the purpose claim being
// checked — not merely set — anyone holding any valid token from this system could present it here and
// have it exchanged for a session, which is precisely the confusion that makes a second factor
// decorative.
//
// The token below is shaped like the ones business/User mints: a subject, an expiry, the right key, and
// no purpose.
func TestParseTOTPChallengeRejectsASessionToken(t *testing.T) {
	withSecret(t)
	userID := uuid.New()

	sessionShaped := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID.String(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := sessionShaped.SignedString([]byte("test-jwt-secret-with-enough-length-1234567890"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("a token with no purpose claim was accepted as a challenge (err=%v)", err)
	}
}

// A token carrying some OTHER purpose is refused too.
//
// Pinned separately from the missing-claim case because the natural implementation bug differs: checking
// `purpose != ""` would pass this, and checking presence rather than value is an easy thing to write.
func TestParseTOTPChallengeRejectsAWrongPurpose(t *testing.T) {
	withSecret(t)

	other := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     uuid.New().String(),
		"purpose": "password_reset",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := other.SignedString([]byte("test-jwt-secret-with-enough-length-1234567890"))

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("a token with purpose=password_reset was accepted (err=%v)", err)
	}
}

// An expired challenge is refused.
//
// The window exists so a user can find their phone; it must not be unbounded, or a challenge captured
// from a browser stays spendable indefinitely.
func TestParseTOTPChallengeRejectsAnExpiredToken(t *testing.T) {
	withSecret(t)

	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     uuid.New().String(),
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(-time.Minute).Unix(),
	})
	signed, _ := expired.SignedString([]byte("test-jwt-secret-with-enough-length-1234567890"))

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("an expired challenge was accepted (err=%v)", err)
	}
}

// A challenge signed with a different key is refused.
func TestParseTOTPChallengeRejectsAForeignSignature(t *testing.T) {
	withSecret(t)

	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     uuid.New().String(),
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := forged.SignedString([]byte("a-completely-different-signing-key-000000"))

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("a challenge signed with the wrong key was accepted (err=%v)", err)
	}
}

// An unsigned token is refused.
//
// "alg: none" is the oldest JWT attack there is: strip the signature, declare no algorithm, and hope the
// verifier believes the header. The method is pinned to HS256 for exactly this.
func TestParseTOTPChallengeRejectsAnUnsignedToken(t *testing.T) {
	withSecret(t)

	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub":     uuid.New().String(),
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Skipf("this jwt version refuses to mint an unsigned token at all: %v", err)
	}

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("an alg=none token was accepted (err=%v)", err)
	}
}

// Garbage in, refusal out, without panicking.
//
// Every one of these arrives in a JSON body from an unauthenticated caller, so a panic here is a denial
// of service anyone can trigger.
func TestParseTOTPChallengeRejectsMalformedInput(t *testing.T) {
	withSecret(t)

	for _, raw := range []string{
		"",
		"   ",
		"not-a-token",
		"a.b.c",
		"....",
		"eyJhbGciOiJIUzI1NiJ9",
	} {
		if _, _, err := ParseTOTPChallenge(raw); err == nil {
			t.Errorf("%q must not parse as a challenge", raw)
		}
	}
}

// A challenge whose subject is not a uuid is refused.
//
// The subject is fed to a database lookup, so a non-uuid must be rejected at the boundary rather than
// producing a query with a nonsense parameter.
func TestParseTOTPChallengeRejectsANonUUIDSubject(t *testing.T) {
	withSecret(t)

	odd := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     "not-a-uuid",
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := odd.SignedString([]byte("test-jwt-secret-with-enough-length-1234567890"))

	if _, _, err := ParseTOTPChallenge(signed); !errors.Is(err, ErrTOTPChallengeInvalid) {
		t.Errorf("a non-uuid subject was accepted (err=%v)", err)
	}
}

// With no JWT_SECRET, a challenge can be neither issued nor parsed.
//
// Fail closed. An unset signing key must not degrade to an unsigned or predictable challenge — that
// would make the challenge forgeable, and the challenge is what stands between the password step and a
// session.
func TestTOTPChallengeRequiresASigningKey(t *testing.T) {
	// A WELL-FORMED token signed with a guessable string, so the assertion below distinguishes
	// "refused because there is no key" from "refused because the input was garbage".
	//
	// The first version of this test passed ParseTOTPChallenge the literal "anything" and asserted an
	// error. That proved nothing: unparseable input fails whatever the key is, so the test stayed green
	// when I mutated the implementation to fall back to a default secret — which is precisely the bug it
	// was supposed to forbid. Negative verification is what surfaced that.
	guessable := "fallback"
	plausible := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     uuid.New().String(),
		"purpose": totpChallengePurpose,
		"exp":     time.Now().Add(time.Hour).Unix(),
	})
	signed, err := plausible.SignedString([]byte(guessable))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	t.Setenv("JWT_SECRET", "")

	if _, err := IssueTOTPChallenge(uuid.New(), userModels.AuthMethodEmail); err == nil {
		t.Error("issuing a challenge with no JWT_SECRET must fail rather than sign with a default")
	}
	if _, _, err := ParseTOTPChallenge(signed); err == nil {
		t.Error("with no JWT_SECRET, a token signed with a guessable default was ACCEPTED — the " +
			"implementation is falling back to a hardcoded key, which makes the challenge forgeable")
	}
}
