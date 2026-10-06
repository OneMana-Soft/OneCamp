package business

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	githubLinkDomain "github.com/akashc777/OneCamp/domain/GitHubLink"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// Why this file exists
// --------------------
// Every inbound GitHub webhook event (issues, pull_request, push,
// issue_comment, create, pull_request_review, check_run) hits Postgres
// at least once via githubLinkModel.GetGitHubLinkByRepo to resolve
// "is this repo linked to OneCamp, and which project?". The signature
// verification path also calls it because per-link secrets live on
// that row.
//
// For a busy repo (push storms during a release, large PR review
// threads) this is one DB round-trip per event, sometimes two when
// the event handler also invokes GetGitHubLinkById.
//
// Links change rarely (admin clicks "Link" / "Unlink" in the UI), so
// a 5-minute TTL with explicit invalidation on every CRUD is safe and
// reduces inbound-webhook DB pressure dramatically.
//
// Negative caching
// ----------------
// We also cache "no link" misses with a shorter TTL. Without this,
// any GitHub webhook delivered for a repo that was never linked
// (common: the same GitHub App can be installed on many repos) would
// keep hitting Postgres on every retry. 30s gives operators a quick
// recovery window after they Link a repo without leaking thousands of
// long-lived empty entries.
//
// Generation fencing matches the webhook dispatch cache: any CRUD
// path bumps a global atomic counter and stale entries are rejected
// at lookup time.

const (
	githubLinkPositiveTTL = 5 * time.Minute
	githubLinkNegativeTTL = 30 * time.Second
)

type githubLinkCacheEntry struct {
	link *githubLinkModel.GitHubLink // nil = negative cache
	// parsedRules is the pre-decoded automation rules map. We pre-parse
	// once at cache-fill time so every webhook event handler that needs
	// to apply automation rules avoids a per-event json.Unmarshal of
	// the same JSON blob.
	parsedRules map[string]string
	generation  uint64
}

var (
	githubLinkCache       = helpers.NewTTLCache[githubLinkCacheEntry](githubLinkPositiveTTL)
	githubLinkGeneration  uint64
	githubLinkCacheHits   uint64
	githubLinkCacheMisses uint64
)

// invalidateGitHubLinkCache is called from every link CRUD path. We
// bump the generation counter rather than walking the cache because
// we don't always know which (owner, name) keys are affected (e.g. on
// disconnect we'd have to enumerate every link). A single atomic add
// invalidates everything in O(1) and the TTL guarantees the map
// itself shrinks.
func invalidateGitHubLinkCache() {
	atomic.AddUint64(&githubLinkGeneration, 1)
}

// linkCacheKey encodes "owner/name" with a separator that cannot
// appear inside either component (GitHub disallows '/' in both
// owner login and repo name).
func linkCacheKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// GetGitHubLinkByRepoCached is the cached entry-point the webhook
// handlers should call. The behaviour matches GetGitHubLinkByRepo for
// every caller, including the (nil, nil) "not linked" return.
//
// On generation mismatch or TTL expiry the helper falls through to
// the underlying domain query and stores the fresh result.
func GetGitHubLinkByRepoCached(ctx context.Context, owner, name string) (*githubLinkModel.GitHubLink, map[string]string, error) {
	if owner == "" || name == "" {
		return nil, nil, nil
	}

	gen := atomic.LoadUint64(&githubLinkGeneration)
	key := linkCacheKey(owner, name)

	if entry, ok := githubLinkCache.Get(key); ok && entry.generation == gen {
		atomic.AddUint64(&githubLinkCacheHits, 1)
		return entry.link, entry.parsedRules, nil
	}
	atomic.AddUint64(&githubLinkCacheMisses, 1)

	link, err := githubLinkModel.GetGitHubLinkByRepo(owner, name)
	if err != nil {
		return nil, nil, err
	}

	curGen := atomic.LoadUint64(&githubLinkGeneration)
	rules := parseAutomationRules(link)
	entry := githubLinkCacheEntry{link: link, parsedRules: rules, generation: curGen}

	if link == nil {
		// Short TTL for negative results so a freshly-linked repo
		// becomes visible quickly. We still store it so a flood of
		// webhooks for an unlinked repo doesn't hammer Postgres.
		githubLinkCache.SetWithTTL(key, entry, githubLinkNegativeTTL)
	} else {
		githubLinkCache.Set(key, entry)
	}
	return link, rules, nil
}

// parseAutomationRules pre-decodes the stored automation rules JSON
// once per cache fill. Callers that don't need the rules can ignore
// the second return value.
//
// We swallow JSON parse errors deliberately: malformed rules are not
// fatal to webhook handling — they just mean automation is skipped
// for that link.
func parseAutomationRules(link *githubLinkModel.GitHubLink) map[string]string {
	if link == nil || link.AutomationRules == nil || *link.AutomationRules == "" {
		return nil
	}
	var rules map[string]string
	if err := json.Unmarshal([]byte(*link.AutomationRules), &rules); err != nil {
		// Best-effort: if a future schema change breaks parsing we
		// don't want one bad row to block all webhook handling.
		_ = err
		return nil
	}
	return rules
}

// invalidateLinkByRepo is the targeted invalidation used when we know
// exactly which (owner, name) tuple changed. Combined with the
// generation bump it offers fast-path eviction for the affected key.
func invalidateLinkByRepo(owner, name string) {
	if owner == "" || name == "" {
		invalidateGitHubLinkCache()
		return
	}
	githubLinkCache.Delete(linkCacheKey(owner, name))
	invalidateGitHubLinkCache()
}

// invalidateLinkByLink is a convenience wrapper for callers that have
// the GitHubLink struct in hand (link/unlink paths).
func invalidateLinkByLink(link *githubLinkModel.GitHubLink) {
	if link == nil {
		invalidateGitHubLinkCache()
		return
	}
	invalidateLinkByRepo(link.RepoOwner, link.RepoName)
}

// lookupGitHubLinkByRepo is the (link, error) shim used by callers
// that don't care about pre-parsed automation rules. Keeps the call-
// site change to a single drop-in replacement of githubLinkModel.GetGitHubLinkByRepo.
func lookupGitHubLinkByRepo(ctx context.Context, owner, name string) (*githubLinkModel.GitHubLink, error) {
	link, _, err := GetGitHubLinkByRepoCached(ctx, owner, name)
	return link, err
}

// UpdateAutomationRulesAndInvalidate persists the rules JSON and
// invalidates the cached entry so subsequent webhook handlers pick
// up the new automation map.
func UpdateAutomationRulesAndInvalidate(ctx context.Context, linkId uuid.UUID, rulesJSON string) error {
	link, _ := GetGitHubLinkById(ctx, linkId)
	if err := githubLinkDomain.UpdateAutomationRules(ctx, linkId, rulesJSON); err != nil {
		return err
	}
	invalidateLinkByLink(link)
	return nil
}

// UpdateBranchFormatAndInvalidate persists the branch format and
// invalidates the cached entry.
func UpdateBranchFormatAndInvalidate(ctx context.Context, linkId uuid.UUID, branchFormat string) error {
	link, _ := GetGitHubLinkById(ctx, linkId)
	if err := githubLinkDomain.UpdateBranchFormat(ctx, linkId, branchFormat); err != nil {
		return err
	}
	invalidateLinkByLink(link)
	return nil
}

// GitHubLinkCacheStats returns hit/miss counts and current size for
// observability.
func GitHubLinkCacheStats() (hits, misses uint64, size int) {
	return atomic.LoadUint64(&githubLinkCacheHits), atomic.LoadUint64(&githubLinkCacheMisses), githubLinkCache.Len()
}
