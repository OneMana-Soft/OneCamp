package saml

import (
	"context"
	"errors"
	"fmt"
	"time"

	crewjam "github.com/crewjam/saml"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// ErrAssertionUsed refuses a directory's answer that has already signed someone in.
var ErrAssertionUsed = errors.New("This sign-in was already used. Sign in again.")

// TakeAssertion takes a SAML assertion for one sign-in, and refuses one taken
// before. With AllowIDPInitiated any InResponseTo passes, so a captured
// SAMLResponse (a proxy, a log, a shared machine) could otherwise be posted
// again from any browser while the library still accepts it. That is until
// MaxIssueDelay after its IssueInstant, which crewjam enforces whatever its
// NotOnOrAfter says, so its ID is kept that long; at least a second, since a
// key set with no TTL would never expire.
//
// When Redis can't be asked it refuses: a session can't be started without
// Redis either (business/User StartSession), so no sign-in that would have
// worked is lost, and a replay never gets through.
func TakeAssertion(ctx context.Context, a *crewjam.Assertion) error {
	ttl := max(time.Until(a.IssueInstant.Add(crewjam.MaxIssueDelay)), time.Second)
	first, err := redisStore.SetStringIfAbsent(ctx, registry.SAMLAssertionUsed, []string{a.ID}, "1", ttl)
	if err != nil {
		return fmt.Errorf("checking the SAML assertion is unused: %w", err)
	}
	if !first {
		return ErrAssertionUsed
	}
	return nil
}
