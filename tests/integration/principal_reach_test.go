//go:build integration
// +build integration

package integration_test

// PrincipalCanReach, against a real graph.
//
// The delegation tests next door exercise the same rules through
// AuthorizeDelegation, which covers channels and tasks. This file calls the
// authorizer DIRECTLY, so it can reach the kinds delegation has no surface for —
// projects and docs — and assert the doc rules per grant type rather than in
// aggregate.
//
// Two things here are worth more than the coverage. The project branch reuses the
// exact lookup executeReadProject uses, and this proves that lookup behaves as the
// branch assumes against the real schema. And the doc branch had a bug: it refused
// an author access to their own private document, because it checked the reading,
// editing and commenting lists and nothing guarantees a creator is in any of them.
// That was caught by comparison, fixed, and is asserted here against a real graph
// rather than only in a unit test with a hand-built struct.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPrincipalCanReach -v

import (
	"context"
	"testing"
	"time"

	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestPrincipalCanReachProjectsAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)
	deletedAt := time.Now().UTC().Format(time.RFC3339Nano)

	member := uuid.NewString()
	outsider := uuid.NewString()
	liveProject := uuid.NewString()
	deadProject := uuid.NewString()

	if uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": member, "user_name": "member"},
		// Exists as a user, so a refusal is proven to come from membership rather
		// than from an unresolvable principal.
		{"uid": "_:outsider", "user_uuid": outsider, "user_name": "outsider"},
		{"uid": "_:live", "project_uuid": liveProject, "project_name": "Platform",
			"project_members": []map[string]any{{"uid": "_:member"}}},
		// Archived, with the member's edge left intact — which is the real
		// post-archive state, since archiving removes no memberships.
		{"uid": "_:dead", "project_uuid": deadProject, "project_name": "Sunset",
			"project_members": []map[string]any{{"uid": "_:member"}}, "project_deleted_at": deletedAt},
	}); len(uids) == 0 {
		t.Fatal("seeding assigned no uids")
	}

	ctx := context.Background()
	ref := func(id string) mcpBusiness.ResourceRef {
		return mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceProject, ID: id, Access: mcpBusiness.AccessRead}
	}

	t.Run("a project member is ALLOWED", func(t *testing.T) {
		d := mcpBusiness.PrincipalCanReach(ctx, member, ref(liveProject))
		if !d.Allow {
			t.Fatalf("a project member was refused: %s\n\nThe branch reuses "+
				"GetBasicDgraphProjectInfo and reads project_is_member; if this fails, that "+
				"assumption is wrong against the real schema and the tools would never work.", d.Reason)
		}
		if d.PrincipalDgraphUID == "" {
			t.Error("an allow must carry the resolved principal uid, so the handler never " +
				"resolves the person again and cannot resolve a different one")
		}
	})

	t.Run("a non-member is REFUSED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, outsider, ref(liveProject)); d.Allow {
			t.Fatal("a non-member was granted reach to the project")
		}
	})

	t.Run("an ARCHIVED project is REFUSED even for a member", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, ref(deadProject)); d.Allow {
			t.Fatal("a member was granted reach to an archived project; archiving leaves " +
				"member edges in place, so only a liveness check catches this")
		}
	})

	t.Run("a project that does not exist is REFUSED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, ref(uuid.NewString())); d.Allow {
			t.Fatal("a nonexistent project granted reach; the membership count must not " +
				"default to something truthy when the node is absent")
		}
	})

	t.Run("a missing id is REFUSED rather than widened", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member,
			mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceProject, Access: mcpBusiness.AccessRead}); d.Allow {
			t.Fatal("an object-scoped reference with no id was allowed")
		}
	})
}

func TestPrincipalCanReachDocsAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)

	author := uuid.NewString()
	reader := uuid.NewString()
	outsider := uuid.NewString()
	privateDoc := uuid.NewString()
	publicDoc := uuid.NewString()

	if uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:author", "user_uuid": author, "user_name": "author"},
		{"uid": "_:reader", "user_uuid": reader, "user_name": "reader"},
		{"uid": "_:outsider", "user_uuid": outsider, "user_name": "outsider"},
		// A private doc whose author is DELIBERATELY not in any grant list. That is
		// the shape that exposed the bug: nothing adds a creator to their own doc's
		// reading list, so a rule checking only those lists locks the author out.
		{"uid": "_:private", "doc_uuid": privateDoc, "doc_private": true,
			"doc_created_by":    map[string]any{"uid": "_:author"},
			"doc_reading_users": []map[string]any{{"uid": "_:reader"}}},
		{"uid": "_:public", "doc_uuid": publicDoc, "doc_private": false,
			"doc_created_by": map[string]any{"uid": "_:author"}},
	}); len(uids) == 0 {
		t.Fatal("seeding assigned no uids")
	}

	ctx := context.Background()
	ref := func(id string) mcpBusiness.ResourceRef {
		return mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceDoc, ID: id, Access: mcpBusiness.AccessRead}
	}

	t.Run("the AUTHOR may read their own private doc", func(t *testing.T) {
		d := mcpBusiness.PrincipalCanReach(ctx, author, ref(privateDoc))
		if !d.Allow {
			t.Fatalf("the author of a private doc was refused: %s\n\n"+
				"This is the bug. The author is in none of the grant lists, because nothing "+
				"puts them there. The in-app executor has always counted the creator; the "+
				"MCP rule did not, and would have refused people access to documents they "+
				"wrote.", d.Reason)
		}
	})

	t.Run("an explicit reader may read it", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, reader, ref(privateDoc)); !d.Allow {
			t.Fatalf("a granted reader was refused: %s", d.Reason)
		}
	})

	t.Run("someone with no grant is REFUSED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, outsider, ref(privateDoc)); d.Allow {
			t.Fatal("a person with no grant on a PRIVATE doc was allowed. Without this the " +
				"allows above prove nothing.")
		}
	})

	t.Run("a non-private doc is workspace-visible", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, outsider, ref(publicDoc)); !d.Allow {
			t.Fatalf("a non-private doc was refused to a workspace member: %s", d.Reason)
		}
	})
}

// Conversations and channels.
//
// A DM and a group chat are the same object here — a grouping with a participant
// set — so one branch answers for both and these cases exercise it at both sizes.
//
// The property worth having is that a refusal is now a DECISION. The in-app
// summarize path enforces access by passing the caller's accessible grouping ids
// into the search's permission filter, so a non-participant gets an empty summary:
// safe, but "no messages" and "not allowed" are indistinguishable, so nothing can be
// said to the caller and nothing lands in the audit trail as a refusal. These assert
// the explicit answer.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPrincipalCanReachConversations -v
func TestPrincipalCanReachConversationsAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)
	deletedAt := time.Now().UTC().Format(time.RFC3339Nano)

	member := uuid.NewString()
	partner := uuid.NewString()
	outsider := uuid.NewString()

	dmGrouping := uuid.NewString()
	groupGrouping := uuid.NewString()
	liveChannel := uuid.NewString()
	deadChannel := uuid.NewString()

	if uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": member, "user_name": "member"},
		{"uid": "_:partner", "user_uuid": partner, "user_name": "partner"},
		{"uid": "_:outsider", "user_uuid": outsider, "user_name": "outsider"},

		// A two-person grouping (a DM) and a three-person one (a group chat). Same
		// shape, different size — which is exactly why one rule covers both.
		{"uid": "_:dm", "dm_grouping_id": dmGrouping,
			"dm_participants": []map[string]any{{"uid": "_:member"}, {"uid": "_:partner"}}},
		{"uid": "_:group", "dm_grouping_id": groupGrouping,
			"dm_participants": []map[string]any{{"uid": "_:member"}, {"uid": "_:partner"}, {"uid": "_:outsider"}}},

		{"uid": "_:live_ch", "ch_uuid": liveChannel, "ch_handle": "eng",
			"ch_members": []map[string]any{{"uid": "_:member"}}},
		// Deleted, member edge intact — the real post-deletion state.
		{"uid": "_:dead_ch", "ch_uuid": deadChannel, "ch_handle": "old-eng",
			"ch_members": []map[string]any{{"uid": "_:member"}}, "ch_deleted_at": deletedAt},
	}); len(uids) == 0 {
		t.Fatal("seeding assigned no uids")
	}

	ctx := context.Background()
	chat := func(id string) mcpBusiness.ResourceRef {
		return mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceChat, ID: id, Access: mcpBusiness.AccessRead}
	}
	channel := func(id string) mcpBusiness.ResourceRef {
		return mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceChannel, ID: id, Access: mcpBusiness.AccessRead}
	}

	t.Run("a DM participant is ALLOWED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, chat(dmGrouping)); !d.Allow {
			t.Fatalf("a DM participant was refused: %s", d.Reason)
		}
	})

	t.Run("a non-participant is REFUSED the DM", func(t *testing.T) {
		d := mcpBusiness.PrincipalCanReach(ctx, outsider, chat(dmGrouping))
		if d.Allow {
			t.Fatal("someone outside a two-person conversation was granted reach to it. " +
				"This is the case the in-app path answers with an empty summary rather " +
				"than a refusal, so it is the case worth asserting explicitly.")
		}
		if d.Reason == "" {
			t.Error("a refusal must state a reason, or it cannot be shown to the caller " +
				"or recorded as a refusal rather than an absence")
		}
	})

	t.Run("a group-chat participant is ALLOWED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, outsider, chat(groupGrouping)); !d.Allow {
			t.Fatalf("a participant of the three-person grouping was refused: %s\n\n"+
				"Note this is the same person refused the DM above — so the two results "+
				"together prove the rule is per conversation, not per person.", d.Reason)
		}
	})

	t.Run("a conversation that does not exist is REFUSED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, chat(uuid.NewString())); d.Allow {
			t.Fatal("a nonexistent grouping granted reach; the participation count must " +
				"not default to something truthy when the node is absent")
		}
	})

	t.Run("a channel member is ALLOWED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, channel(liveChannel)); !d.Allow {
			t.Fatalf("a channel member was refused: %s", d.Reason)
		}
	})

	t.Run("a channel non-member is REFUSED", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, outsider, channel(liveChannel)); d.Allow {
			t.Fatal("a non-member was granted reach to the channel")
		}
	})

	t.Run("a DELETED channel is REFUSED even for a member", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, member, channel(deadChannel)); d.Allow {
			t.Fatal("a member was granted reach to a deleted channel; deletion leaves " +
				"member edges in place, so only the liveness check catches this")
		}
	})
}

// Read is not write.
//
// Every case here is a person who legitimately passes the READ check and must still
// be refused the WRITE. That is the whole reason Access exists: before it, a branch
// answered a mutation with whatever its visibility rule said, so a project you could
// see was a project you could restructure and a doc anyone could read was a doc
// anyone could rewrite.
//
// The pairs matter more than the individual assertions. Each subtest asserts the read
// ALLOW and the write REFUSE on the same object for the same person, so neither can
// be passing because the person simply has no access at all.
//
// Run: go test -tags=integration ./tests/integration/ -run TestReadDoesNotImplyWrite -v
func TestReadDoesNotImplyWriteAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)

	plainMember := uuid.NewString()
	projectAdmin := uuid.NewString()
	reader := uuid.NewString()
	editor := uuid.NewString()

	projectUUID := uuid.NewString()
	taskUUID := uuid.NewString()
	publicDoc := uuid.NewString()

	if uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": plainMember, "user_name": "member"},
		{"uid": "_:admin", "user_uuid": projectAdmin, "user_name": "padmin"},
		{"uid": "_:reader", "user_uuid": reader, "user_name": "reader"},
		{"uid": "_:editor", "user_uuid": editor, "user_name": "editor"},

		// Both are members; only one is an admin.
		{"uid": "_:project", "project_uuid": projectUUID, "project_name": "Platform",
			"project_members": []map[string]any{{"uid": "_:member"}, {"uid": "_:admin"}},
			"project_admins":  []map[string]any{{"uid": "_:admin"}}},
		{"uid": "_:task", "task_uuid": taskUUID, "task_name": "Ship it",
			"task_project": map[string]any{"uid": "_:project"}},

		// A NON-PRIVATE doc: readable workspace-wide. Exactly the case where
		// visibility must not imply the right to rewrite.
		{"uid": "_:doc", "doc_uuid": publicDoc, "doc_private": false,
			"doc_reading_users": []map[string]any{{"uid": "_:reader"}},
			"doc_editing_users": []map[string]any{{"uid": "_:editor"}}},
	}); len(uids) == 0 {
		t.Fatal("seeding assigned no uids")
	}

	ctx := context.Background()
	ref := func(kind mcpBusiness.ResourceKind, id string, access mcpBusiness.Access) mcpBusiness.ResourceRef {
		return mcpBusiness.ResourceRef{Kind: kind, ID: id, Access: access}
	}

	t.Run("a plain project member may READ but not WRITE the project", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, plainMember,
			ref(mcpBusiness.ResourceProject, projectUUID, mcpBusiness.AccessRead)); !d.Allow {
			t.Fatalf("a member was refused the read: %s", d.Reason)
		}
		if d := mcpBusiness.PrincipalCanReach(ctx, plainMember,
			ref(mcpBusiness.ResourceProject, projectUUID, mcpBusiness.AccessWrite)); d.Allow {
			t.Fatal("a plain member was granted WRITE on the project. Membership grants " +
				"sight of the work; restructuring it is a project-admin action.")
		}
	})

	t.Run("a project admin may WRITE it", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, projectAdmin,
			ref(mcpBusiness.ResourceProject, projectUUID, mcpBusiness.AccessWrite)); !d.Allow {
			t.Fatalf("a project admin was refused the write: %s\n\nWithout this the "+
				"refusal above could be passing because writes are refused for everyone.", d.Reason)
		}
	})

	t.Run("a plain member may READ but not WRITE a task in that project", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, plainMember,
			ref(mcpBusiness.ResourceTask, taskUUID, mcpBusiness.AccessRead)); !d.Allow {
			t.Fatalf("a project member was refused the task read: %s", d.Reason)
		}
		if d := mcpBusiness.PrincipalCanReach(ctx, plainMember,
			ref(mcpBusiness.ResourceTask, taskUUID, mcpBusiness.AccessWrite)); d.Allow {
			t.Fatal("a plain member was granted WRITE on a task. Seeing a task on a board " +
				"does not carry the right to reassign it.")
		}
	})

	t.Run("a NON-PRIVATE doc is readable by all and writable only by its editors", func(t *testing.T) {
		// The reader is not on the editing list. Before Access existed, the doc being
		// non-private short-circuited to allow — so this person could have rewritten it.
		if d := mcpBusiness.PrincipalCanReach(ctx, reader,
			ref(mcpBusiness.ResourceDoc, publicDoc, mcpBusiness.AccessRead)); !d.Allow {
			t.Fatalf("a workspace-visible doc was refused for reading: %s", d.Reason)
		}
		if d := mcpBusiness.PrincipalCanReach(ctx, reader,
			ref(mcpBusiness.ResourceDoc, publicDoc, mcpBusiness.AccessWrite)); d.Allow {
			t.Fatal("a non-editor was granted WRITE on a workspace-visible doc. A doc " +
				"everyone can read is not a doc everyone can rewrite — and the read rule " +
				"allows this person, so only a separate write rule can refuse them.")
		}
		if d := mcpBusiness.PrincipalCanReach(ctx, editor,
			ref(mcpBusiness.ResourceDoc, publicDoc, mcpBusiness.AccessWrite)); !d.Allow {
			t.Fatalf("a listed editor was refused the write: %s", d.Reason)
		}
	})

	t.Run("a workspace-scoped WRITE is refused outright", func(t *testing.T) {
		if d := mcpBusiness.PrincipalCanReach(ctx, projectAdmin,
			mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceWorkspace, Access: mcpBusiness.AccessWrite}); d.Allow {
			t.Fatal("a workspace-scoped write was allowed. There is no object to check " +
				"against, so such a write is a mutation nobody approved of anything specific.")
		}
	})
}
