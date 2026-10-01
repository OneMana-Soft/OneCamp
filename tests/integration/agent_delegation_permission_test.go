//go:build integration
// +build integration

package integration_test

// The delegation permission check, against a real graph.
//
// WHAT THIS PROVES THAT NOTHING ELSE DID. Agent-to-agent delegation is gated by
// AuthorizeDelegation. Its budget half (hops, cycles, orphaned chains) is a pure
// function covered exhaustively by unit tests. Its PERMISSION half asks Dgraph
// whether the originating PERSON is a member of the channel — or of the project
// owning the task — and that is the check enforcing the invariant that a delegated
// run may never reach an agent the originating person could not have addressed
// themselves.
//
// Every unit test that reaches it runs with no Dgraph configured, so the lookup
// fails and the function returns false. They prove it DENIES. Until this file,
// nothing proved it ALLOWS a legitimate hop, and nothing verified that
// count(ch_members @filter(uid($user))) and the project equivalent are the right
// predicates against the real schema. A guard whose allow path has never executed
// is a guard nobody has actually seen work.
//
// The asymmetry matters: because those queries fail closed, an error in them shows
// up as delegation silently NOT working rather than as an over-permit. That is the
// safe direction, but "fails safe" was an argument, not evidence. This is evidence.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDelegationPermission -v

import (
	"context"
	"testing"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// onCfg is delegation fully enabled for the surface, so the only thing that can
// refuse a hop in these tests is the permission check itself. Budget rules are
// unit-tested; mixing them in here would let a budget denial masquerade as a
// permission denial and the test would pass for the wrong reason.
func onCfg() agentBusiness.DelegationConfig {
	return agentBusiness.DelegationConfig{
		Enabled:        true,
		ChannelOptIn:   true,
		MaxHops:        4,
		MaxChainAgents: 8,
	}
}

// delegatedHop is an AGENT-authored message mid-chain, which is the only shape that
// reaches the permission check. A human-authored message short-circuits to allow.
func delegatedHop(originUserUUID string) agentBusiness.DelegationContext {
	return agentBusiness.DelegationContext{
		AuthorAgentID: uuid.NewString(), // the delegating agent
		OriginUserID:  originUserUUID,   // the person at the root of the chain
		Hop:           1,
		Chain:         nil,
	}
}

func TestDelegationPermissionAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)

	memberUUID := uuid.NewString()
	outsiderUUID := uuid.NewString()
	channelUUID := uuid.NewString()
	projectUUID := uuid.NewString()
	taskUUID := uuid.NewString()

	// One graph shape covering both surfaces:
	//   member   is in ch_members AND project_members
	//   outsider is in neither, but exists as a user (so a denial is proven to come
	//            from membership, not from an unresolvable principal)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": memberUUID, "user_name": "member"},
		{"uid": "_:outsider", "user_uuid": outsiderUUID, "user_name": "outsider"},
		{
			"uid":        "_:channel",
			"ch_uuid":    channelUUID,
			"ch_handle":  "eng-triage",
			"ch_members": []map[string]any{{"uid": "_:member"}},
		},
		{
			"uid":             "_:project",
			"project_uuid":    projectUUID,
			"project_name":    "Platform",
			"project_members": []map[string]any{{"uid": "_:member"}},
		},
		{
			"uid":          "_:task",
			"task_uuid":    taskUUID,
			"task_name":    "Fix the thing",
			"task_project": map[string]any{"uid": "_:project"},
		},
	})
	if len(uids) == 0 {
		t.Fatal("seeding assigned no uids; the mutation did not take effect")
	}
	t.Logf("seeded uids: %v", uids)

	ctx := context.Background()
	agentTarget := uuid.NewString()

	channelSurface := agentBusiness.Surface{
		Kind:      agentBusiness.SurfaceChannelPost,
		ChannelID: channelUUID,
		PostID:    "post-1",
	}
	taskSurface := agentBusiness.Surface{Kind: agentBusiness.SurfaceTask}

	t.Run("channel member: a delegated hop is ALLOWED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(memberUUID), agentTarget, channelSurface, "post-1")
		if !d.Allow {
			t.Fatalf("a channel member's chain was refused: %s\n\n"+
				"This is the case that had no coverage. Either the membership query "+
				"or the graph shape it expects is wrong, and delegation would never "+
				"work in production even when correctly configured.", d.Reason)
		}
	})

	t.Run("channel non-member: the hop is REFUSED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(outsiderUUID), agentTarget, channelSurface, "post-1")
		if d.Allow {
			t.Fatal("a chain rooted in a NON-MEMBER was allowed into the channel. " +
				"This is privilege laundering: an agent would act on a surface the " +
				"originating person cannot see, with its owner's authority.")
		}
	})

	t.Run("project member: a delegated hop on the task is ALLOWED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(memberUUID), agentTarget, taskSurface, taskUUID)
		if !d.Allow {
			t.Fatalf("a project member's chain was refused on its own task: %s", d.Reason)
		}
	})

	t.Run("project non-member: the hop on the task is REFUSED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(outsiderUUID), agentTarget, taskSurface, taskUUID)
		if d.Allow {
			t.Fatal("a chain rooted in a non-member of the project was allowed to " +
				"act on its task")
		}
	})

	t.Run("a person who does not exist at all is REFUSED", func(t *testing.T) {
		// Distinct from "not a member": an unresolvable principal must also deny,
		// and must do so without being mistaken for a member.
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(uuid.NewString()), agentTarget, channelSurface, "post-1")
		if d.Allow {
			t.Fatal("a chain rooted in a nonexistent user was allowed")
		}
	})

	t.Run("a channel that does not exist is REFUSED even for a real member", func(t *testing.T) {
		// Guards against the membership count defaulting to something truthy when
		// the channel node is absent.
		absent := agentBusiness.Surface{
			Kind:      agentBusiness.SurfaceChannelPost,
			ChannelID: uuid.NewString(),
			PostID:    "post-1",
		}
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(memberUUID), agentTarget, absent, "post-1")
		if d.Allow {
			t.Fatal("a nonexistent channel authorised a delegated hop")
		}
	})

	t.Run("a human-authored message never depends on membership", func(t *testing.T) {
		// The outsider posting for themselves is authorised by the act of posting;
		// delegation rules must not gate a direct human mention.
		dc := agentBusiness.DelegationContext{AuthorAgentID: "", OriginUserID: outsiderUUID}
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(), dc, agentTarget, channelSurface, "post-1")
		if !d.Allow {
			t.Fatalf("a human mention was refused: %s", d.Reason)
		}
	})
}
