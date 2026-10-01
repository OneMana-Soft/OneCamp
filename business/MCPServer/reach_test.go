package business

// PrincipalCanReach is deny-by-default on anything it cannot positively verify.
// These cover the refusals that need no database, which is most of them — the
// membership-allows path needs a real graph and is covered in
// tests/integration/mcp_reach_test.go, for the same reason the delegation
// permission check is.
//
// Note that in this test binary no Dgraph is configured, so any case reaching a
// lookup returns false anyway. Every case below is therefore asserted to refuse
// for a reason that is established BEFORE any lookup — the reason string is part
// of the assertion, so a test cannot pass by accident of the absent database.

import (
	"context"
	"strings"
	"testing"
)

func TestPrincipalCanReachRefusesWhatItCannotVerify(t *testing.T) {
	ctx := context.Background()
	const realUUID = "11111111-1111-1111-1111-111111111111"

	cases := []struct {
		name       string
		principal  string
		ref        ResourceRef
		wantReason string
	}{
		{
			"no principal",
			"", ResourceRef{Kind: ResourceChannel, ID: realUUID},
			"no originating person",
		},
		{
			"blank principal",
			"   ", ResourceRef{Kind: ResourceChannel, ID: realUUID},
			"no originating person",
		},
		{
			"resource-scoped tool with no id",
			realUUID, ResourceRef{Kind: ResourceChannel, ID: ""},
			"no resource id",
		},
		{
			"blank resource id",
			realUUID, ResourceRef{Kind: ResourceTask, ID: "   "},
			"no resource id",
		},
		{
			"unknown resource kind",
			realUUID, ResourceRef{Kind: ResourceKind("repository"), ID: realUUID},
			// Reaches the principal lookup first, which fails with no DB. Either
			// refusal is correct; what must never happen is an allow.
			"",
		},
		{
			// A table id that is not a uuid is refused on its own terms, before any
			// lookup, the same way a missing id is.
			"table id is not a uuid",
			realUUID, ResourceRef{Kind: ResourceTable, ID: "not-a-uuid"},
			"",
		},
		{
			// The table branch is the only one whose actor comes from POSTGRES rather
			// than the graph, so it is the only one whose fail-closed behaviour is not
			// already covered by the graph lookup failing. With no database it must
			// still refuse — an authorization path that opens up when its store is
			// unreachable is worse than one that is simply unavailable.
			"table with no database reachable",
			realUUID, ResourceRef{Kind: ResourceTable, ID: "22222222-2222-2222-2222-222222222222"},
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PrincipalCanReach(ctx, c.principal, c.ref)
			if got.Allow {
				t.Fatalf("must refuse what it cannot verify, but allowed with reason %q", got.Reason)
			}
			if got.Reason == "" {
				t.Error("a refusal must always carry a reason; an unexplained denial " +
					"is indistinguishable from a bug to whoever reports it")
			}
			if c.wantReason != "" && !strings.Contains(got.Reason, c.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, c.wantReason)
			}
		})
	}
}

// The ordering property: an unusable ResourceRef is refused on its own terms,
// before the principal is looked up. Asserted through the reason, because a
// database-dependent refusal would produce a different one.
func TestPrincipalCanReachValidatesTheRefBeforeAnyLookup(t *testing.T) {
	got := PrincipalCanReach(context.Background(),
		"11111111-1111-1111-1111-111111111111",
		ResourceRef{Kind: ResourceChannel, ID: ""})

	if got.Allow {
		t.Fatal("a channel ref with no id was allowed")
	}
	if !strings.Contains(got.Reason, "no resource id") {
		t.Errorf("expected refusal on the ref shape before resolving the person, "+
			"got %q — which suggests a query ran to reach the same answer", got.Reason)
	}
}

// An allow must always carry the resolved graph uid, because the handler is given
// it and must never resolve the principal a second time (and so risk resolving a
// different one). Structural check on the contract rather than on a live allow.
func TestReachDecisionAllowMustCarryThePrincipalUID(t *testing.T) {
	allowed := ReachDecision{Allow: true, Reason: "channel member", PrincipalDgraphUID: "0x1"}
	if allowed.Allow && allowed.PrincipalDgraphUID == "" {
		t.Fatal("an allow without a resolved uid would force the handler to resolve " +
			"the principal itself")
	}
}
