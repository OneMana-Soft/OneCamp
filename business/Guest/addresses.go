package business

// What a kind's address answers now, said without serving it (docs C6-2,
// agreed with CALLS-LEAD): a doc's page tells the web's /p/<slug> which frame
// to show, and a room's lobby asks before the knock. The same check as
// serving it (ResolveExternal, then the kind's own, StateFor), so the two
// never disagree, and nothing more is said than the address itself would
// say. A personal room's address resolves once and asks StateFor, as it also
// reads its passcode's unlock (business/CallRoom At).
//
// Every kind's public reads share one per-address limit
// (redisStore.AllowPerAddress), counted by surface (Kind.Surface), so each
// kind counts apart.

import (
	"errors"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
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
