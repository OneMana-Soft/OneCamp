package business

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
)

// An event only some may know of goes to the workspace's webhooks whose owner
// mayKnow allows, asking once per owner, and to no other.
func TestAnEventOnlySomeMayKnowOfReachesOnlyTheirWebhooks(t *testing.T) {
	reader, stranger := uuid.New(), uuid.New()
	hooks := []*webhookModel.Webhook{
		{Id: uuid.New(), Name: "reader's", CreatedBy: reader, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "stranger's", CreatedBy: stranger, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "reader's other", CreatedBy: reader, ScopeType: ScopeOrg},
	}
	names := map[uuid.UUID]string{}
	for _, w := range hooks {
		names[w.Id] = w.Name
	}
	saved := queueDeliveries
	t.Cleanup(func() { queueDeliveries = saved; dispatchCache.Purge() })
	var sent []string
	queueDeliveries = func(_ context.Context, _ uuid.UUID, _ string, targets []hooksBusiness.Target) error {
		for _, tg := range targets {
			sent = append(sent, names[tg.WebhookID])
		}
		return nil
	}

	dispatchCache.Purge()
	dispatchCache.Set(dispatchCacheKey("doc.shared", ScopeOrg, nil), dispatchCacheEntry{
		generation: atomic.LoadUint64(&dispatchGeneration),
		webhooks:   hooks,
	})
	asked := map[uuid.UUID]int{}
	DispatchEventFor(context.Background(), "doc.shared", map[string]interface{}{"doc_uuid": uuid.NewString()},
		func(_ context.Context, owner uuid.UUID) bool {
			asked[owner]++
			return owner == reader
		})
	if len(sent) != 2 || sent[0] != "reader's" || sent[1] != "reader's other" {
		t.Fatalf("sent to %v", sent)
	}
	if asked[reader] != 1 || asked[stranger] != 1 {
		t.Fatalf("owners asked %v", asked)
	}

	// Without the check, every webhook is sent it, as before.
	sent = nil
	DispatchEvent(context.Background(), "doc.shared", map[string]interface{}{"doc_uuid": uuid.NewString()})
	if len(sent) != 3 {
		t.Fatalf("an event for all went to %v", sent)
	}
}
