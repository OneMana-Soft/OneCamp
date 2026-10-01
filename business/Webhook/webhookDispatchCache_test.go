package business

import (
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
)

// dispatch cache key collision tests — the (event_type, scope_type,
// scope_entity_id) tuple must be unambiguously reversible because two
// scopes with overlapping ids would silently fan out to the wrong
// subscribers.
func TestDispatchCacheKeyDistinct(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	keys := []string{
		dispatchCacheKey("post.created", "org", nil),
		dispatchCacheKey("post.created", "channel", &id1),
		dispatchCacheKey("post.created", "channel", &id2),
		dispatchCacheKey("post.updated", "channel", &id1),
		dispatchCacheKey("post.created", "project", &id1),
	}

	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("dispatchCacheKey collision: %q", k)
		}
		seen[k] = true
	}
}

// bumpDispatchCacheVersion must invalidate previously-stored entries
// even though we use lazy invalidation. After a bump, a stored entry
// from the old generation should not be returned.
func TestDispatchCacheGenerationFencing(t *testing.T) {
	dispatchCache.Purge()
	startGen := atomic.LoadUint64(&dispatchGeneration)

	key := dispatchCacheKey("test.event", "org", nil)
	dispatchCache.Set(key, dispatchCacheEntry{
		webhooks:   []*webhookModel.Webhook{{Name: "first"}},
		generation: startGen,
	})

	bumpDispatchCacheVersion()

	// Even though the entry is in the map, the generation check
	// inside the cached helper should treat it as a miss. We model
	// the check directly here because the helper makes a real DB
	// call on miss.
	entry, ok := dispatchCache.Get(key)
	if !ok {
		t.Fatalf("entry missing in raw cache before generation check")
	}
	if entry.generation == atomic.LoadUint64(&dispatchGeneration) {
		t.Fatalf("entry generation should be stale after bump")
	}
}
