package business

import (
	"context"
	"testing"
	"time"

	capabilityModels "github.com/akashc777/OneCamp/models/postgres/Capability"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Can is the single authorization entry point for every delegatable action, and
// it had no tests at all. These cover the decisions it makes BEFORE it consults
// the database, which is both the security-critical part and the part that can be
// tested without one: who is refused outright, and who is allowed outright.
//
// The database-backed branch (policy lookup for an ordinary member) is not
// exercised here. It is covered by the model's own tests plus the integration
// suite; what matters at this layer is that no caller can reach that branch when
// they should have been refused before it.

func userWith(admin, bot, external bool) *userModels.UserInfo {
	return &userModels.UserInfo{
		UserPostgresInfo: userModels.User{
			Id:         uuid.New(),
			IsAdmin:    admin,
			IsBot:      bot,
			IsExternal: external,
		},
	}
}

// aCapability is a real entry from the catalog, so the test cannot pass by
// accident on a name the system does not recognise.
func aCapability(t *testing.T) string {
	t.Helper()
	if len(capabilityModels.AllCapabilities) == 0 {
		t.Skip("no capabilities registered")
	}
	return capabilityModels.AllCapabilities[0]
}

func TestCanRefusesWithoutAUser(t *testing.T) {
	// A nil user is what a handler passes when authentication has not populated
	// the context. Answering "true" there would be an authentication bypass.
	if Can(context.Background(), nil, aCapability(t)) {
		t.Error("Can returned true for a nil user")
	}
}

func TestCanRefusesBotsAndExternalUsersWithoutReachingThePolicy(t *testing.T) {
	// These two are refused before any policy lookup, which is why they can be
	// asserted with no database. If the short-circuit is ever moved below the
	// lookup, this test starts failing by panicking on a nil DB rather than
	// quietly returning the policy's answer.
	for _, c := range []struct {
		name string
		user *userModels.UserInfo
	}{
		{"bot", userWith(false, true, false)},
		{"external", userWith(false, false, true)},
		{"external bot", userWith(false, true, true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if Can(context.Background(), c.user, aCapability(t)) {
				t.Errorf("%s was granted a delegated capability", c.name)
			}
		})
	}
}

func TestAdminsAreAllowedEverything(t *testing.T) {
	admin := userWith(true, false, false)

	// Including a capability that is not in the catalog. An admin short-circuits
	// before resolution, so an unknown name must not fall through to the
	// fail-closed path and must not touch the database.
	for _, capability := range []string{aCapability(t), "capability.that.does.not.exist"} {
		if !Can(context.Background(), admin, capability) {
			t.Errorf("admin refused capability %q", capability)
		}
	}
}

// An account can be both admin and bot. Admin is checked first, so it wins.
// Pinning it because it is a real decision rather than an accident: an
// admin-flagged bot is trusted, and if that is ever wrong this test names it.
func TestAdminBeatsBotWhenAnAccountIsBoth(t *testing.T) {
	if !Can(context.Background(), userWith(true, true, false), aCapability(t)) {
		t.Error("an admin bot was refused; if that is intended, change this test deliberately")
	}
}

func TestInvalidateDropsTheCachedPolicy(t *testing.T) {
	const capability = "test.capability.invalidate"

	policyMu.Lock()
	policyCache[capability] = cachedPolicy{policy: capabilityModels.PolicyAllMembers, at: time.Now()}
	policyMu.Unlock()

	invalidate(capability)

	policyMu.RLock()
	_, present := policyCache[capability]
	policyMu.RUnlock()

	if present {
		t.Error("invalidate left the entry cached; a revoked capability would stay granted until the TTL expired")
	}
}

func TestSetPolicyRejectsBadInputBeforeWriting(t *testing.T) {
	// Validation happens before the database call, so these return without one.
	// The point is that an unrecognised capability or policy can never be
	// persisted, which would otherwise put a value in the table that resolves
	// fail-closed forever with no way to see why.
	admin := *userWith(true, false, false)

	if err := SetPolicy(context.Background(), "not.a.capability", capabilityModels.PolicyAllMembers, admin); err == nil {
		t.Error("SetPolicy accepted an unknown capability")
	}
	if err := SetPolicy(context.Background(), aCapability(t), "not_a_policy", admin); err == nil {
		t.Error("SetPolicy accepted an unknown policy value")
	}
}

// The catalog is what Settings renders and what MyCapabilities iterates. A
// duplicate or an empty entry there is a UI bug and a policy row that can never
// be set.
func TestCapabilityCatalogIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range capabilityModels.AllCapabilities {
		if c == "" {
			t.Error("empty capability in the catalog")
		}
		if seen[c] {
			t.Errorf("duplicate capability %q in the catalog", c)
		}
		seen[c] = true
		if !capabilityModels.ValidCapability(c) {
			t.Errorf("catalog lists %q but ValidCapability rejects it", c)
		}
	}
}
