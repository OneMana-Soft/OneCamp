package authService

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// GenerateAndStoreSSOState mints a random state token, persists it in
// Redis under registry.SSOState (registered TTL = 5 minutes), and
// returns the token. The token is what we hand the IdP; the redirect
// URL never leaves our control.
//
// The 15m TTL the BE used to declare here was an outlier vs. the
// rest of the codebase (most OIDC libraries default to 5–10 min); we
// now use the 5m registry default which is well below the IdP's own
// MFA timeout in every flow we tested.
func GenerateAndStoreSSOState(ctx context.Context, redirectURL string) (string, error) {
	if !redisStore.IsAvailable() {
		return "", fmt.Errorf("redis unavailable; cannot mint sso state")
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	state := hex.EncodeToString(buf)

	// Bound the Redis call so the login redirect doesn't stall on a
	// misbehaving Redis.
	rctx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	defer cancel()

	if err := redisStore.SetString(rctx, registry.SSOState, []string{state}, redirectURL); err != nil {
		helpers.LogErrorWithContext(ctx, "services/Auth GenerateAndStoreSSOState set err: %+v", err)
		return "", err
	}

	return state, nil
}

// ConsumeSSOState looks up the state in Redis, deletes it (single-use),
// and returns the redirect URL that was stored at mint time. Returns
// an error if the state is unknown / already consumed / expired.
func ConsumeSSOState(ctx context.Context, state string) (string, error) {
	if !redisStore.IsAvailable() {
		return "", fmt.Errorf("redis unavailable; cannot consume sso state")
	}

	if state == "" {
		return "", fmt.Errorf("empty sso state")
	}

	// Bound state length so a malicious client can't make us hash a giant
	// key on every callback. Our minted states are 64 hex chars; 256 is a
	// generous ceiling.
	if len(state) > 256 {
		return "", fmt.Errorf("sso state too long")
	}

	rctx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	defer cancel()

	val, hit, err := redisStore.GetDelString(rctx, registry.SSOState, []string{state})
	if err != nil {
		return "", fmt.Errorf("sso state lookup failed: %w", err)
	}
	if !hit {
		return "", fmt.Errorf("sso state expired or already used")
	}
	return val, nil
}

// redisCallTimeout bounds individual Redis ops in the login path. Long
// enough that healthy Redis isn't affected, short enough to keep latency
// predictable when Redis is slow.
const redisCallTimeout = 500 * time.Millisecond
