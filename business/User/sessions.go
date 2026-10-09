package business

// Sessions: each device's refresh token, traded for a new one on every use,
// and ended everywhere when a password is reset.

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

var (
	// ErrRefreshUnknown is a device with no session here (signed out, or
	// ended by a password reset).
	ErrRefreshUnknown = errors.New("no session for this device")
	// ErrRefreshReused is a refresh token used again after it was traded for
	// a new one: someone else holds a copy, so the device is signed out.
	ErrRefreshReused = errors.New("a refresh token was used after it was rotated")
)

// RotateRefreshToken trades the refresh token a device presented for a new
// access and refresh token. The refresh token it had stays good for a while
// after (registry.UserRefreshTokenPrev): another tab may have sent it as this
// one refreshed, or the answer to this refresh may never have arrived.
// Presented then, it gets a new access token and the device's current refresh
// token. Any other refresh token signs the device out.
//
// One step in Redis (store.RotateString): read, compared and rotated
// separately, two tabs refreshing at once could both rotate, leaving the
// browser holding one token and the server another, and the device was signed
// out at its next refresh.
//
// The stored token used to be read and never compared, so rotation changed
// nothing: a refresh token, once copied, kept working for its month.
func RotateRefreshToken(ctx context.Context, userID, deviceID, presented string, authExp, refreshExp int64) (string, string, error) {
	next, err := GenerateRefreshTokenString(ctx, userID, refreshExp)
	if err != nil {
		return "", "", err
	}
	found, current, err := redisStore.RotateString(ctx, registry.UserRefreshToken, registry.UserRefreshTokenPrev,
		[]string{userID, deviceID}, presented, next)
	if err != nil {
		return "", "", err
	}
	switch found {
	case redisStore.RotateDone, redisStore.RotatePrevious:
		auth, err := GenerateAuthTokenString(ctx, userID, authExp)
		return auth, current, err
	case redisStore.RotateUnknown:
		return "", "", ErrRefreshUnknown
	default:
		return "", "", ErrRefreshReused
	}
}

// EndAllSessions signs a user out on every device, as when their password is
// reset: what a stolen refresh token could do ends with it.
func EndAllSessions(ctx context.Context, userID string) error {
	return errors.Join(
		redisStore.DeletePattern(ctx, registry.UserRefreshToken.Pattern(userID)),
		redisStore.DeletePattern(ctx, registry.UserRefreshTokenPrev.Pattern(userID)),
	)
}

// EndOtherSessions signs a user out on every device but keepDevice, as when
// they change their password: they stay signed in where they changed it.
// The kept device's tokens are never touched, so a refresh it makes meanwhile
// still finds them.
func EndOtherSessions(ctx context.Context, userID, keepDevice string) error {
	args := []string{userID, keepDevice}
	return errors.Join(
		redisStore.DeletePatternExcept(ctx, registry.UserRefreshToken.Pattern(userID), registry.UserRefreshToken.Build(args...)),
		redisStore.DeletePatternExcept(ctx, registry.UserRefreshTokenPrev.Pattern(userID), registry.UserRefreshTokenPrev.Build(args...)),
	)
}
