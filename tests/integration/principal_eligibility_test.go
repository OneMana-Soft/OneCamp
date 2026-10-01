//go:build integration
// +build integration

package integration_test

// Eligibility, against a real graph.
//
// WHAT THIS PROVES. The delegation permission test next door establishes that
// membership is checked correctly. This one establishes something membership cannot
// express: that an identity which IS a member can still be refused.
//
// Every principal seeded below is a genuine member of the channel and the project.
// The only difference between the allowed case and the refused ones is a flag or a
// timestamp on the user node, or on the resource. So a passing run here is direct
// evidence that eligibility is enforced independently of membership — and a failing
// one cannot be explained away as a graph-shape problem, because the control case
// shares the shape exactly.
//
// WHY IT NEEDS A REAL DGRAPH. The gap this closes was invisible precisely because
// every unit test runs with no graph attached, where the lookup fails and the
// answer is a denial for the wrong reason. Deactivation leaves membership edges
// intact, so the only way to demonstrate that an offboarded person is refused is to
// have a real member node carrying a real deleted_at and watch the refusal happen.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPrincipalEligibility -v

import (
	"context"
	"testing"
	"time"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestPrincipalEligibilityAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)

	// A real deletion timestamp, in the format the schema stores.
	deletedAt := time.Now().UTC().Format(time.RFC3339Nano)
	// What ActivateUser writes when it brings an account back: not NULL, but the
	// zero time. A reactivated member must be eligible again, and a naive nil check
	// would lock them out permanently.
	reactivatedAt := time.Time{}.UTC().Format(time.RFC3339Nano)

	active := uuid.NewString()
	deactivated := uuid.NewString()
	reactivated := uuid.NewString()
	bot := uuid.NewString()
	external := uuid.NewString()

	liveChannel := uuid.NewString()
	deadChannel := uuid.NewString()
	projectUUID := uuid.NewString()
	taskUUID := uuid.NewString()
	deadProject := uuid.NewString()
	taskInDeadProject := uuid.NewString()

	// EVERY user below is a member of both the channel and the project. That is the
	// point: the control and the refusals differ only in eligibility.
	allMembers := []map[string]any{
		{"uid": "_:active"}, {"uid": "_:deactivated"}, {"uid": "_:reactivated"},
		{"uid": "_:bot"}, {"uid": "_:external"},
	}

	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:active", "user_uuid": active, "user_name": "active"},
		{"uid": "_:deactivated", "user_uuid": deactivated, "user_name": "offboarded", "user_deleted_at": deletedAt},
		{"uid": "_:reactivated", "user_uuid": reactivated, "user_name": "rehired", "user_deleted_at": reactivatedAt},
		{"uid": "_:bot", "user_uuid": bot, "user_name": "assistant", "is_bot": true},
		{"uid": "_:external", "user_uuid": external, "user_name": "ghost", "is_external": true},

		{"uid": "_:live_channel", "ch_uuid": liveChannel, "ch_handle": "eng", "ch_members": allMembers},
		{"uid": "_:dead_channel", "ch_uuid": deadChannel, "ch_handle": "archived-eng",
			"ch_members": allMembers, "ch_deleted_at": deletedAt},

		{"uid": "_:project", "project_uuid": projectUUID, "project_name": "Platform", "project_members": allMembers},
		{"uid": "_:task", "task_uuid": taskUUID, "task_name": "Live task", "task_project": map[string]any{"uid": "_:project"}},

		{"uid": "_:dead_project", "project_uuid": deadProject, "project_name": "Sunset",
			"project_members": allMembers, "project_deleted_at": deletedAt},
		{"uid": "_:task_dead_project", "task_uuid": taskInDeadProject, "task_name": "Orphan",
			"task_project": map[string]any{"uid": "_:dead_project"}},
	})
	if len(uids) == 0 {
		t.Fatal("seeding assigned no uids; the mutation did not take effect")
	}

	ctx := context.Background()
	agentTarget := uuid.NewString()

	channel := func(id string) agentBusiness.Surface {
		return agentBusiness.Surface{Kind: agentBusiness.SurfaceChannelPost, ChannelID: id, PostID: "post-1"}
	}
	taskSurface := agentBusiness.Surface{Kind: agentBusiness.SurfaceTask}

	// THE CONTROL. If this fails, every refusal below is meaningless, because the
	// graph shape rather than the gate would be doing the work.
	t.Run("an active member is ALLOWED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(active), agentTarget, channel(liveChannel), "post-1")
		if !d.Allow {
			t.Fatalf("an active member of the channel was refused: %s\n\n"+
				"Every other case in this file is seeded identically apart from one flag, "+
				"so if the control is refused nothing here proves anything.", d.Reason)
		}
	})

	t.Run("a DEACTIVATED member is REFUSED despite intact membership", func(t *testing.T) {
		// The finding this whole change exists for. DeactivateUser writes a
		// timestamp and stops: it removes no membership edges and revokes no
		// tokens, so this user is still in ch_members right now.
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(deactivated), agentTarget, channel(liveChannel), "post-1")
		if d.Allow {
			t.Fatal("a DEACTIVATED person's delegation chain was authorised. They are " +
				"still in ch_members because deactivation does not remove membership " +
				"edges, so offboarding an employee would leave their agents running " +
				"with their authority.")
		}
	})

	t.Run("a REACTIVATED member is ALLOWED again", func(t *testing.T) {
		// Guards the other direction. ActivateUser writes the zero time rather than
		// clearing the field, so treating any non-null timestamp as a deletion
		// would permanently lock out anyone who has ever been deactivated.
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(reactivated), agentTarget, channel(liveChannel), "post-1")
		if !d.Allow {
			t.Fatalf("a REACTIVATED member was refused: %s\n\n"+
				"ActivateUser writes the zero time, not NULL. Reading that as a deletion "+
				"locks out every rehired or reinstated account.", d.Reason)
		}
	})

	t.Run("a BOT identity is REFUSED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(bot), agentTarget, channel(liveChannel), "post-1")
		if d.Allow {
			t.Fatal("a chain rooted at a BOT was authorised. There is no accountable " +
				"person behind it, so the audit trail would name a bot as the authority " +
				"for the work.")
		}
	})

	t.Run("an EXTERNAL attribution identity is REFUSED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(external), agentTarget, channel(liveChannel), "post-1")
		if d.Allow {
			t.Fatal("a chain rooted at an is_external ghost identity was authorised. " +
				"Those identities exist only so history can name an author and cannot " +
				"sign in, so none of them can have intended a call.")
		}
	})

	t.Run("a DELETED channel is REFUSED for an active member", func(t *testing.T) {
		// Deleting a channel does not clear ch_members, so the membership count
		// still reports this person as a member of a channel nobody can open.
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(active), agentTarget, channel(deadChannel), "post-1")
		if d.Allow {
			t.Fatal("an active member was authorised to act on a DELETED channel; " +
				"deletion leaves membership edges in place, so only a liveness check " +
				"can catch this")
		}
	})

	t.Run("a task in a DELETED project is REFUSED for an active member", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(active), agentTarget, taskSurface, taskInDeadProject)
		if d.Allow {
			t.Fatal("an active member was authorised to act on a task whose PROJECT is " +
				"deleted; archiving a project leaves its tasks addressable")
		}
	})

	// Sanity: the task path still allows the legitimate case, so the two task
	// assertions above and below cannot both be passing for the same wrong reason.
	t.Run("a task in a live project is still ALLOWED", func(t *testing.T) {
		d := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(active), agentTarget, taskSurface, taskUUID)
		if !d.Allow {
			t.Fatalf("an active project member was refused on a live task: %s", d.Reason)
		}
	})
}

// Restore round-trip: archiving is reversible, and so is the refusal.
//
// WHY THIS MATTERS AS MUCH AS THE REFUSAL. OneCamp soft-deletes almost everything
// so it can be brought back: business/Archive restores posts, chats, tasks, docs,
// recordings and attachments, and all six Dgraph restore paths write the GO ZERO
// TIME back into the deleted_at predicate rather than removing it.
//
// That makes the encoding load-bearing in both directions. A liveness check written
// as `deletedAt != nil` would refuse a restored resource forever — the restore would
// appear to succeed, the item would come back in the UI, and agents would keep being
// refused on it with no way for an operator to tell why. Denying access to something
// that was brought back is a quieter failure than granting access to something that
// was removed, which is exactly why it is worth a test.
//
// The property being demonstrated is that access follows the CURRENT state of the
// object on every call. There is no cache to invalidate and no credential to
// re-issue: the same principal, the same token, the same surface, refused and then
// allowed, with nothing changing but the timestamp on the resource.
//
// Run: go test -tags=integration ./tests/integration/ -run TestArchiveRestore -v
func TestArchiveRestoreReGrantsAccessAgainstRealGraph(t *testing.T) {
	dg := integration.SetupDgraph(t)

	archivedAt := time.Now().UTC().Format(time.RFC3339Nano)
	// Exactly what BulkRestoreTasksInDgraph, BulkRestoreDocs and the other four
	// restore paths write: the Go zero time, not a cleared predicate.
	restoredAt := time.Time{}.UTC().Format(time.RFC3339Nano)

	member := uuid.NewString()
	channelUUID := uuid.NewString()
	projectUUID := uuid.NewString()
	taskUUID := uuid.NewString()

	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": member, "user_name": "member"},
		// Both resources start ARCHIVED, with the member's access intact — which is
		// the real post-archive state, since archiving removes no edges.
		{"uid": "_:channel", "ch_uuid": channelUUID, "ch_handle": "eng",
			"ch_members": []map[string]any{{"uid": "_:member"}}, "ch_deleted_at": archivedAt},
		{"uid": "_:project", "project_uuid": projectUUID, "project_name": "Platform",
			"project_members": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:task", "task_uuid": taskUUID, "task_name": "Archived work",
			"task_project": map[string]any{"uid": "_:project"}, "task_deleted_at": archivedAt},
	})

	channelUID, taskUID := uids["channel"], uids["task"]
	if channelUID == "" || taskUID == "" {
		t.Fatalf("seeding did not return uids for the archived nodes: %v", uids)
	}

	ctx := context.Background()
	agentTarget := uuid.NewString()
	channelSurface := agentBusiness.Surface{
		Kind: agentBusiness.SurfaceChannelPost, ChannelID: channelUUID, PostID: "post-1",
	}
	taskSurface := agentBusiness.Surface{Kind: agentBusiness.SurfaceTask}

	// Restoring is a single predicate write, exactly as the restore paths do it.
	restore := func(uid, predicate string) {
		t.Helper()
		dg.Mutate(t, []map[string]any{{"uid": uid, predicate: restoredAt}})
	}

	t.Run("channel", func(t *testing.T) {
		before := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(member), agentTarget, channelSurface, "post-1")
		if before.Allow {
			t.Fatal("an ARCHIVED channel granted access before restore; the archive state " +
				"is not being observed at all, so the rest of this test proves nothing")
		}

		restore(channelUID, "ch_deleted_at")

		after := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(member), agentTarget, channelSurface, "post-1")
		if !after.Allow {
			t.Fatalf("a RESTORED channel is still refused: %s\n\n"+
				"Restore writes the Go zero time back into ch_deleted_at rather than "+
				"clearing the predicate. Any liveness check that reads a non-nil "+
				"timestamp as a deletion makes unarchiving permanently ineffective for "+
				"agents, while the channel looks restored everywhere else.", after.Reason)
		}
	})

	t.Run("task", func(t *testing.T) {
		before := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(member), agentTarget, taskSurface, taskUUID)
		if before.Allow {
			t.Fatal("an ARCHIVED task granted access before restore")
		}

		restore(taskUID, "task_deleted_at")

		after := agentBusiness.AuthorizeDelegation(ctx, onCfg(),
			delegatedHop(member), agentTarget, taskSurface, taskUUID)
		if !after.Allow {
			t.Fatalf("a RESTORED task is still refused: %s\n\n"+
				"business/Archive lists tasks as restorable and BulkRestoreTasksInDgraph "+
				"writes the zero time, so this is the supported admin flow, not an edge "+
				"case.", after.Reason)
		}
	})
}
