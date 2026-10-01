package business

// The delegation permission check must FAIL CLOSED, not crash.
//
// originCanAddressSurface reaches a graph client that is nil until the server has
// connected, and the driver dereferences it without checking. So asking an
// authorization question while the graph is unavailable panics instead of refusing.
//
// That matters in production, not just in tests. A delegated agent hop arriving
// during a datastore blip would panic inside the mention dispatcher rather than
// declining the hop. The correct behaviour for a security decision is always the
// refusal: an outage should degrade to "no agent can act", never to "the process
// serving humans dies".
//
// The existing layering test hints at this — its comment says a call reaching the
// lookup "would fail or panic rather than allow" — and treats the panic as
// acceptable because it is not an allow. It is not acceptable: a panic in a
// dispatcher goroutine is an availability bug, and it is reachable with no
// database attached.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestAuthorizeDelegationFailsClosedWhenTheGraphIsUnavailable(t *testing.T) {
	// A hop that passes every budget rule, so the permission check is definitely
	// reached: agent-authored, a real originating person, within hops, no cycle.
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 4, MaxChainAgents: 8}
	dc := DelegationContext{
		AuthorAgentID: uuid.NewString(),
		OriginUserID:  uuid.NewString(),
		Hop:           1,
	}
	surface := Surface{
		Kind:      SurfaceChannelPost,
		ChannelID: uuid.NewString(),
		PostID:    "post-1",
	}

	// No Dgraph is configured in this binary. This must return a refusal, not panic.
	d := AuthorizeDelegation(context.Background(), cfg, dc, uuid.NewString(), surface, "post-1")

	if d.Allow {
		t.Fatal("a hop was allowed with no permission graph available; the check must " +
			"never allow what it cannot verify")
	}
	if d.Reason == "" {
		t.Error("a refusal must carry a reason")
	}
}

func TestOriginCanAddressSurfaceFailsClosedForEverySurfaceKind(t *testing.T) {
	// Each of these reaches the principal lookup, so each is a chance to panic.
	for _, s := range []struct {
		name    string
		surface Surface
		entity  string
	}{
		{"channel", Surface{Kind: SurfaceChannelPost, ChannelID: uuid.NewString()}, "post-1"},
		{"task", Surface{Kind: SurfaceTask}, uuid.NewString()},
	} {
		t.Run(s.name, func(t *testing.T) {
			if originCanAddressSurface(context.Background(), uuid.NewString(), s.surface, s.entity) {
				t.Fatal("must not authorise what it cannot verify")
			}
		})
	}
}
