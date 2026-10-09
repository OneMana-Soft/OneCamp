package helpers

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func signClaims(t *testing.T, key string, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestASessionTokenIsTheKindItSaysAndNothingElseIs(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret-with-enough-entropy-0123456789")
	user := uuid.New()
	soon, later := time.Now().Add(5*time.Minute).Unix(), time.Now().Add(30*24*time.Hour).Unix()

	access, err := SignSessionToken(user.String(), TokenTypeAccess, soon)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := SignSessionToken(user.String(), TokenTypeRefresh, later)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseSessionToken(access, TokenTypeAccess); err != nil || got != user {
		t.Fatalf("an access token as access: %v %v", got, err)
	}
	// Two minted in the same second are still two tokens.
	if again, _ := SignSessionToken(user.String(), TokenTypeRefresh, later); again == refresh {
		t.Fatal("two refresh tokens minted together are the same string")
	}
	if got, err := ParseSessionToken(refresh, TokenTypeRefresh); err != nil || got != user {
		t.Fatalf("a refresh token as refresh: %v %v", got, err)
	}

	secret := "test-secret-with-enough-entropy-0123456789"
	for name, tc := range map[string]struct {
		token string
		want  string
	}{
		// The month-long refresh token signed requests in.
		"a refresh token as a session": {refresh, TokenTypeAccess},
		"an access token as a refresh": {access, TokenTypeRefresh},
		// A password alone was a session: the two-step challenge.
		"a two-step challenge":              {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": soon, "purpose": "totp_challenge"}), TokenTypeAccess},
		"an old refresh token as a session": {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": later}), TokenTypeAccess},
		"an old access token as a refresh":  {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": soon}), TokenTypeRefresh},
		"a guest's collaboration token":     {signClaims(t, secret, jwt.MapClaims{"sub": "guest-" + user.String(), "exp": soon}), TokenTypeAccess},
		"another kind":                      {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": soon, "typ": "mqtt"}), TokenTypeAccess},
		"no expiry":                         {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "typ": TokenTypeAccess}), TokenTypeAccess},
		"expired":                           {signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": time.Now().Add(-time.Minute).Unix(), "typ": TokenTypeAccess}), TokenTypeAccess},
		"another key":                       {signClaims(t, "another-secret", jwt.MapClaims{"sub": user.String(), "exp": soon, "typ": TokenTypeAccess}), TokenTypeAccess},
		"no subject":                        {signClaims(t, secret, jwt.MapClaims{"exp": soon, "typ": TokenTypeAccess}), TokenTypeAccess},
	} {
		if _, err := ParseSessionToken(tc.token, tc.want); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Tokens minted before they said their kind: an old refresh token still
	// refreshes (which mints typed tokens), and no old token is a session.
	if _, err := ParseSessionToken(signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": later}), TokenTypeRefresh); err != nil {
		t.Errorf("an old refresh token: %v", err)
	}
	if _, err := ParseSessionToken(signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": soon}), TokenTypeAccess); err == nil {
		t.Error("an old access token was a session")
	}
	// An old refresh token in the last minutes of its month: not a session
	// either (it used to be, until expiry, password resets and all).
	lastMinutes := time.Now().Add(5 * time.Minute).Unix()
	if _, err := ParseSessionToken(signClaims(t, secret, jwt.MapClaims{"sub": user.String(), "exp": lastMinutes}), TokenTypeAccess); err == nil {
		t.Error("an old refresh token near its end was a session")
	}
	// An "alg: none" token is refused.
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": user.String(), "exp": soon, "typ": TokenTypeAccess}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := ParseSessionToken(none, TokenTypeAccess); err == nil {
		t.Error("an unsigned token was accepted")
	}
}
