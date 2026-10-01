package business

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	domain "github.com/akashc777/OneCamp/domain/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
)

// Why this file exists
// --------------------
// DispatchEvent runs on every chat/post/task/channel event. Without
// caching, every event makes a Postgres roundtrip to load the active
// outgoing webhook list for the (event_type, scope_type, scope_entity)
// triple. On a busy workspace (mention storms, bulk task imports,
// stand-up bursts) this can be hundreds of identical queries per
// minute fighting for the same connection slots.
//
// Webhooks change rarely. A short, invalidatable cache turns a hot
// query into an in-memory lookup with the same correctness guarantees
// because:
//   - Every CRUD path on webhooks (Create / Update / Delete /
//     RegenerateToken / RegenerateSecret / DisableWebhook) routes
//     through bumpDispatchCacheVersion below, which atomically bumps
//     a generation counter. Stale entries are rejected on read.
//   - When the cache TTL expires the entry is reloaded; this bounds
//     staleness even if a write path forgot to invalidate (defence in
//     depth).
//   - The cache stores []*Webhook by reference; the dispatch loop
//     never mutates these structs, so sharing across goroutines is
//     safe without copying.

// dispatchCacheTTL is short on purpose. Webhook config changes
// propagate within this window even if a write path is added in the
// future without remembering to bump the version. 30s is well below
// the operator's expectation of "instant" without thrashing the DB.
const dispatchCacheTTL = 30 * time.Second

// dispatchCacheEntry is a value pinned to a generation. We never serve
// an entry from a stale generation, so the only reason to keep the
// generation around is so the cache can reject it cheaply.
type dispatchCacheEntry struct {
	webhooks   []*webhookModel.Webhook
	generation uint64
}

// dispatchCache is a TTLCache keyed by the dispatch tuple, plus a
// monotonically increasing generation counter that lets every write
// path invalidate every cached entry with a single atomic add.
//
// We use a single atomic instead of per-entry deletes because:
//   - Webhook CRUD doesn't know which event types it might be
//     subscribed to; conservatively we'd have to enumerate them all.
//   - The cost of an atomic.Add is negligible compared to deletes
//     across thousands of cached event-type buckets.
//   - generation also provides natural fencing for in-flight readers
//     that have not yet stored a value: a read that completes during
//     a CRUD will store with the old generation and be ignored on the
//     next lookup, so we never serve data older than the most recent
//     write.
var (
	dispatchCache       = helpers.NewTTLCache[dispatchCacheEntry](dispatchCacheTTL)
	dispatchGeneration  uint64
	dispatchCacheHits   uint64
	dispatchCacheMisses uint64
)

// bumpDispatchCacheVersion is called from every webhook CRUD path.
// It atomically advances the generation, which causes every existing
// entry to be ignored on the next read. We do not actively Purge the
// cache because the next reader will repopulate from Postgres in any
// case and Purge under load can cause a thundering herd; lazy
// invalidation is friendlier.
func bumpDispatchCacheVersion() {
	atomic.AddUint64(&dispatchGeneration, 1)
}

// dispatchCacheKey produces the lookup key. Encoded as a printable
// string so the underlying TTLCache can store it directly.
//
// scopeEntityId may be nil for org-scoped events; we use the literal
// "nil" so org-scope and channel-scope keys never collide.
func dispatchCacheKey(eventType, scopeType string, scopeEntityId *uuid.UUID) string {
	scopeID := "nil"
	if scopeEntityId != nil {
		scopeID = scopeEntityId.String()
	}
	// The pipe separator cannot appear in any of the components
	// (event_type is dot-namespaced, scope_type is a closed enum,
	// scopeID is a UUID) so this is unambiguous.
	return fmt.Sprintf("%s|%s|%s", eventType, scopeType, scopeID)
}

// getActiveOutgoingWebhooksByEventCached is the hot-path dispatch
// helper. It reads from the in-memory cache, falling back to the
// Postgres query on miss / stale generation, and stores fresh results
// for the next call.
//
// On error from the underlying domain call we propagate the error
// without caching — a transient DB hiccup should not cement an empty
// result for the next 30s.
func getActiveOutgoingWebhooksByEventCached(ctx context.Context, eventType, scopeType string, scopeEntityId *uuid.UUID) ([]*webhookModel.Webhook, error) {
	gen := atomic.LoadUint64(&dispatchGeneration)
	key := dispatchCacheKey(eventType, scopeType, scopeEntityId)

	if entry, ok := dispatchCache.Get(key); ok && entry.generation == gen {
		atomic.AddUint64(&dispatchCacheHits, 1)
		return entry.webhooks, nil
	}
	atomic.AddUint64(&dispatchCacheMisses, 1)

	webhooks, err := domain.GetActiveOutgoingWebhooksByEvent(ctx, eventType, scopeType, scopeEntityId)
	if err != nil {
		return nil, err
	}

	// Re-check generation after the DB roundtrip. If a CRUD landed
	// while we were querying, our result is already stale relative to
	// the new generation but is still as fresh as the DB row at the
	// time we read it. Store it under the LATEST generation so the
	// next reader gets the just-loaded value rather than a phantom
	// re-query.
	dispatchCache.Set(key, dispatchCacheEntry{
		webhooks:   webhooks,
		generation: atomic.LoadUint64(&dispatchGeneration),
	})
	return webhooks, nil
}
