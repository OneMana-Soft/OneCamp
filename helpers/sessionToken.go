package helpers

// Session tokens: what signing in hands a browser, and the one check every
// middleware that reads one makes.
//
// Every token this server signs used the same key and, for sessions, the same
// two claims (sub and exp). So an access token, a month-long refresh token and
// a two-step sign-in's challenge were interchangeable: a refresh token signed
// requests in for a month, whatever logging out or the refresh rotation did,
// and a password alone (the challenge) was a session, skipping the second
// step. A session token now says what it is (typ), and each check asks for
// one kind.

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// TokenTypeAccess signs a request in.
	TokenTypeAccess = "access"
	// TokenTypeRefresh only gets a new access token.
	TokenTypeRefresh = "refresh"
	// legacyAccessWindow tells the two apart in tokens minted before they
	// said what they are: an access token lived about six minutes and a
	// refresh token a month, so an untyped token running out after this
	// window is an old refresh token, which may still refresh. No untyped
	// token is a session: an old access token gets a 401, and the app
	// refreshes once, which mints typed ones. (Telling an old access token
	// by its expiry let an old refresh token sign requests in for the last
	// ten minutes of its month, a password reset notwithstanding.)
	legacyAccessWindow = 10 * time.Minute
)

// ErrNotASession is a token that isn't a session of the kind asked for.
var ErrNotASession = errors.New("not a valid session token")

// SignSessionToken mints a session token of kind typ for a user, running out
// at exp.
func SignSessionToken(userID string, typ string, exp int64) (string, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return "", errors.New("JWT_SECRET is not configured")
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": exp,
		"iat": time.Now().Unix(),
		"typ": typ,
		// Every token its own: two minted for one person in the same second
		// were otherwise the same string, so a rotation could hand back the
		// token it was trading.
		"jti": uuid.NewString(),
	}).SignedString([]byte(secret))
}

// ParseSessionToken checks a session token (its signature, algorithm, expiry
// and kind) and answers the user it was issued to. want is the kind asked for.
// A token minted for anything else (a two-step challenge, a guest's
// collaboration token) is never either kind.
func ParseSessionToken(raw string, want string) (uuid.UUID, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return uuid.Nil, ErrNotASession
	}
	token, err := jwt.Parse(raw, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return uuid.Nil, ErrNotASession
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return uuid.Nil, ErrNotASession
	}
	if _, has := claims["purpose"]; has {
		return uuid.Nil, ErrNotASession // a two-step challenge, or anything else with a purpose
	}
	switch typ, _ := claims["typ"].(string); typ {
	case "":
		if want != TokenTypeRefresh {
			return uuid.Nil, ErrNotASession
		}
		exp, err := claims.GetExpirationTime()
		if err != nil || exp == nil || time.Until(exp.Time) <= legacyAccessWindow {
			return uuid.Nil, ErrNotASession
		}
	case want:
	default:
		return uuid.Nil, ErrNotASession
	}
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, ErrNotASession
	}
	return id, nil
}
