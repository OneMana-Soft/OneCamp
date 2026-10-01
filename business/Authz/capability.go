// Package authz is the generic, reusable authorization layer for delegatable
// capabilities. A capability (create workflows, invite members, manage apps, …)
// is either restricted to admins or open to all members; admins set the policy
// in workspace Settings. Features ask authz.Can(...) instead of hard-coding an
// admin check, so the same policy engine governs every delegatable action and
// adding one is a one-line constant.
//
// Fail-closed by design: an unknown capability, a DB error, or a missing policy
// row all resolve to admins_only.
package business

import (
	"context"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	capabilityModels "github.com/akashc777/OneCamp/models/postgres/Capability"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// policyCacheTTL bounds how stale a cached policy can be. Policies change rarely
// (an admin toggles them in Settings); a short TTL keeps the permission check
// off the DB on the hot path while making a change take effect promptly.
const policyCacheTTL = 30 * time.Second

type cachedPolicy struct {
	policy string
	at     time.Time
}

var (
	policyMu    sync.RWMutex
	policyCache = map[string]cachedPolicy{}
)

// resolvePolicy returns a capability's policy, served from a short-lived cache.
func resolvePolicy(ctx context.Context, capability string) string {
	policyMu.RLock()
	if c, ok := policyCache[capability]; ok && time.Since(c.at) < policyCacheTTL {
		policyMu.RUnlock()
		return c.policy
	}
	policyMu.RUnlock()

	policy, err := capabilityModels.GetPolicy(ctx, capability)
	if err != nil {
		// Fail-closed: on a read error, treat as admins_only and do NOT cache
		// the error result (so a transient blip doesn't pin the restrictive
		// value for the whole TTL).
		return capabilityModels.PolicyAdminsOnly
	}

	policyMu.Lock()
	policyCache[capability] = cachedPolicy{policy: policy, at: time.Now()}
	policyMu.Unlock()
	return policy
}

// invalidate drops a capability's cached policy so a change is reflected at the
// next check without waiting out the TTL.
func invalidate(capability string) {
	policyMu.Lock()
	delete(policyCache, capability)
	policyMu.Unlock()
}

// Can reports whether the given user may exercise a capability. Admins can
// always exercise every capability; non-admins can only when the capability's
// policy is all_members. This is the single authorization entry point features
// should call.
func Can(ctx context.Context, userInfo *userModels.UserInfo, capability string) bool {
	if userInfo == nil {
		return false
	}
	if userInfo.UserPostgresInfo.IsAdmin {
		return true
	}
	// Bots and external/ghost users are never granted delegated capabilities.
	if userInfo.UserPostgresInfo.IsBot || userInfo.UserPostgresInfo.IsExternal {
		return false
	}
	return resolvePolicy(ctx, capability) == capabilityModels.PolicyAllMembers
}

// ListPolicies returns the full capability policy catalog (admin Settings UI).
func ListPolicies(ctx context.Context) ([]capabilityModels.CapabilityPolicy, error) {
	return capabilityModels.ListPolicies(ctx)
}

// MyCapabilities returns a map of every known capability → whether the given
// user may exercise it. Reusable by the FE to gate UI for any capability-gated
// feature (an unknown future capability simply appears with its resolved bool).
func MyCapabilities(ctx context.Context, userInfo *userModels.UserInfo) map[string]bool {
	out := make(map[string]bool, len(capabilityModels.AllCapabilities))
	for _, cap := range capabilityModels.AllCapabilities {
		out[cap] = Can(ctx, userInfo, cap)
	}
	return out
}

// SetPolicy updates a capability policy (admin only — enforced at the route).
// Validates inputs and invalidates the cache so the change is immediate.
func SetPolicy(ctx context.Context, capability, policy string, updatedBy userModels.UserInfo) error {
	if !capabilityModels.ValidCapability(capability) {
		return errInvalidCapability
	}
	if !capabilityModels.ValidPolicy(policy) {
		return errInvalidPolicy
	}
	if err := capabilityModels.SetPolicy(ctx, capability, policy, updatedBy.UserPostgresInfo.Id); err != nil {
		return err
	}
	invalidate(capability)
	helpers.LogInfoWithContext(ctx, "authz: capability %q set to %q by %s", capability, policy, updatedBy.UserPostgresInfo.Id)
	return nil
}
