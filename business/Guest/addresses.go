package business

// What a kind's address answers now, said without serving it (docs C6-2,
// agreed with CALLS-LEAD): a doc's page tells the web's /p/<slug> which frame
// to show, and a room's lobby asks before the knock. The same check as
// serving it (ResolveExternal, then the kind's own, StateFor), so the two
// never disagree, and nothing more is said than the address itself would
// say. A caller that resolves the address itself (the room's lobby) adds
// its own StateOf with it: an exported function nobody calls yet is dead
// (helpers/deadExportedFuncGuard_test.go).
//
// AddressAllowed is the one per-address limit on every kind's public reads,
// counted by surface (Kind.Surface), so each kind counts apart.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// What an address answers.
const (
	StateLive    = "live"
	StateLocked  = "locked"  // it has a password
	StateGone    = "gone"    // it expired
	StateMissing = "missing" // never published, taken down, another kind's, or off
)

// StateFor is what an address answers for kind k, given what resolving it
// gave (ResolveExternal's grant and error); a read that failed is returned as
// it is, never a state. Pure.
func StateFor(k Kind, g *guestModel.GuestGrant, err error) (string, error) {
	switch {
	case errors.Is(err, ErrGone):
		return StateGone, nil
	case errors.Is(err, ErrGuestDisabled), errors.Is(err, ErrInvalidGrant), errors.Is(err, ErrNotFound):
		return StateMissing, nil
	case err != nil:
		return "", err
	case !k.Serves(g):
		return StateMissing, nil
	case g.PasswordHash != nil:
		return StateLocked, nil
	}
	return StateLive, nil
}

// AddressAllowed counts one ask of surface from ip (an address as
// helpers.ClientIP gives it) and reports whether it is within perMinute. An
// ask with no address is let through; a counter that can't be reached in
// half a second counts locally.
func AddressAllowed(ctx context.Context, surface, ip string, perMinute int) bool {
	if ip == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return redisStore.AllowFixedWindowOrLocal(ctx, registry.PublicReadRate, []string{surface, helpers.RateKeyIP(ip)}, perMinute).Allowed
}
