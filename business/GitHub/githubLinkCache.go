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
// issue_comment, create, pull_request_review, check_run) reads the links to
// its repository: whether any project links it, and whether any of those
// links syncs what the event is about. The signature verification path reads
// them too because per-link secrets live on those rows.
//
// For a busy repo (push storms during a release, large PR review
// threads) this is one DB round-trip per event.
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
	links      []*githubLinkModel.GitHubLink // none = negative cache
	generation uint64
}

var (
	githubLinkCache      = helpers.NewTTLCache[githubLinkCacheEntry](githubLinkPositiveTTL)
	githubLinkGeneration uint64
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

// repositoryLinks is every project's live link to the repository owner/name,
// the oldest first, matched in any case as GitHub matches names; none when no
// project links it. Several projects may link one repository (the unique
// index is per project), so no one of their links speaks for the others: what
// an event does to a task is its own project's link's to decide (eventTask),
// and what it does without a task needs one of them to sync that
// (linkSyncing). The webhook handlers and the signature check read them here.
//
// On generation mismatch or TTL expiry it reads them again and stores them.
func repositoryLinks(ctx context.Context, owner, name string) ([]*githubLinkModel.GitHubLink, error) {
	if owner == "" || name == "" {
		return nil, nil
	}

	// The generation from before the read: an invalidation while the links
	// are read leaves the entry stale, never current with what it replaced.
	gen := atomic.LoadUint64(&githubLinkGeneration)
	key := linkCacheKey(owner, name)

	if entry, ok := githubLinkCache.Get(key); ok && entry.generation == gen {
		return entry.links, nil
	}

	links, err := githubLinkModel.GetGitHubLinksByRepo(ctx, owner, name)
	if err != nil {
		return nil, err
	}

	entry := githubLinkCacheEntry{links: links, generation: gen}
	if len(links) == 0 {
		// Short TTL for negative results so a freshly-linked repo
		// becomes visible quickly. We still store it so a flood of
		// webhooks for an unlinked repo doesn't hammer Postgres.
		githubLinkCache.SetWithTTL(key, entry, githubLinkNegativeTTL)
	} else {
		githubLinkCache.Set(key, entry)
	}
	return links, nil
}

// ProjectsLinking are the projects that link the repository owner/name, matched
// in any case as GitHub matches names, from the links the webhooks read; none
// when no project links it.
func ProjectsLinking(ctx context.Context, owner, name string) ([]uuid.UUID, error) {
	links, err := repositoryLinks(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	projects := make([]uuid.UUID, 0, len(links))
	for _, l := range links {
		projects = append(projects, l.ProjectId)
	}
	return projects, nil
}

// parseAutomationRules decodes a link's stored automation rules JSON.
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
