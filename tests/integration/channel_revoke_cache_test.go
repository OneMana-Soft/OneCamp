//go:build integration
// +build integration

package integration_test

// Does removing somebody from a channel actually take their access away?
//
// WHAT THIS PROVES THAT NOTHING ELSE DID. ChannelBasicInfo caches ch_is_member and
// ch_is_admin for thirty minutes and sits behind roughly two dozen authorization
// gates: create a post, open a channel, an agent's send_message, the admins_only
// check. Adding a member purged that cache. Removing one did not. So granting
// access was instant and REVOKING IT TOOK UP TO HALF AN HOUR -- an admin could
// remove somebody from a private channel, watch the roster update, and that person
// could still read it and still post, because every gate was asking a cache that
// had not been told.
//
// helpers/channelRevokeCacheGuard_test.go walks the source and fails if a revoke
// path does not CALL the purge. That is worth having and it is not evidence: it
// cannot tell whether the purge addresses the key the read uses. The key is built
// from the channel UUID and the viewer's Dgraph uid, and passing the wrong one
// would satisfy the walk perfectly while changing nothing. This executes the
// sequence instead.
//
// THE TEST MUST HAVE A REAL CACHE OR IT PROVES NOTHING. Every store operation is
// gated on a non-nil Redis client, so without a container the warm-up writes
// nothing, the read after removal is a miss, Dgraph answers correctly, and the
// test passes against the unfixed code. Hence SetupRedis.
//
// Run: go test -tags=integration ./tests/integration/ -run TestRevoke -v

import (
	"context"
	"testing"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// wireStores brings up everything the revoke path touches.
//
// Postgres is needed even though the assertion is about Dgraph and Redis:
// DeleteChannelMemberEdge fires a goroutine that clears the removed person's
// notification preferences, and with no pool that goroutine dereferences a nil
// *sql.DB and takes the whole test binary down with it. Discovered by this test
// crashing rather than failing, which is its own small argument for running the
// real path instead of a stub of it.
func wireStores(t *testing.T) *integration.DgraphEnv {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	return integration.SetupDgraph(t)
}

func TestRevokingChannelMembershipTakesEffectImmediately(t *testing.T) {
	dg := wireStores(t)
	ctx := context.Background()

	memberUUID := uuid.NewString()
	channelUUID := uuid.New()

	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": memberUUID, "user_name": "member"},
		// A private channel the person is genuinely in, so the warm-up below
		// caches a YES and the removal has something to undo.
		{"uid": "_:ch", "ch_uuid": channelUUID.String(), "ch_name": "finance",
			"ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:member"}}},
	})
	memberDgraphUID, channelDgraphUID := uids["member"], uids["ch"]
	if memberDgraphUID == "" || channelDgraphUID == "" {
		t.Fatalf("seeding assigned no uids: %+v", uids)
	}

	// 1. Warm the cache the way a gate does: by answering "can this person post
	//    here?" through the same function every gate calls.
	before, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, memberDgraphUID)
	if err != nil {
		t.Fatalf("reading channel before removal: %v", err)
	}
	if before == nil || before.IsMember != 1 {
		t.Fatalf("the fixture is wrong: the person is not a member to begin with (%+v), "+
			"so removing them would prove nothing", before)
	}

	// 2. Remove them, through the call the admin's own "remove member" button uses.
	if err := channelBusiness.DeleteChannelMemberEdge(
		ctx, channelDgraphUID, memberDgraphUID, memberUUID, channelUUID.String()); err != nil {
		t.Fatalf("removing the member: %v", err)
	}

	// 3. Ask again. This is the moment the bug lived in: the edge is gone from the
	//    graph and the answer every gate gets comes from Redis.
	after, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, memberDgraphUID)
	if err != nil {
		t.Fatalf("reading channel after removal: %v", err)
	}
	if after == nil {
		t.Fatal("the channel read nothing after the removal; the channel itself should still exist")
	}
	if after.IsMember != 0 {
		t.Fatalf("ch_is_member is still %d after the person was removed.\n"+
			"The membership cache was not invalidated, so every permission gate "+
			"(create post, open channel, the agent's send_message, the admins_only check) "+
			"still answers YES for up to the 30 minute TTL. A removed person keeps "+
			"reading and posting in a private channel.", after.IsMember)
	}
}

func TestDemotingAChannelAdminTakesEffectImmediately(t *testing.T) {
	dg := wireStores(t)
	ctx := context.Background()

	adminUUID := uuid.NewString()
	channelUUID := uuid.New()

	// Moderator AND member, which is the real shape: ch_is_admin is a separate
	// count and the announcement-channel gate reads only that one.
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:admin", "user_uuid": adminUUID, "user_name": "chadmin"},
		{"uid": "_:ch", "ch_uuid": channelUUID.String(), "ch_name": "announcements",
			"ch_private": true, "ch_post_policy": "admins_only",
			"ch_members":    []map[string]any{{"uid": "_:admin"}},
			"ch_moderators": []map[string]any{{"uid": "_:admin"}}},
	})
	adminDgraphUID, channelDgraphUID := uids["admin"], uids["ch"]
	if adminDgraphUID == "" || channelDgraphUID == "" {
		t.Fatalf("seeding assigned no uids: %+v", uids)
	}

	before, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, adminDgraphUID)
	if err != nil {
		t.Fatalf("reading channel before demotion: %v", err)
	}
	if before == nil || before.IsAdmin != 1 {
		t.Fatalf("the fixture is wrong: not a channel admin to begin with (%+v)", before)
	}

	if err := channelBusiness.DeleteChannelModeratorEdge(
		ctx, channelDgraphUID, adminDgraphUID, channelUUID.String()); err != nil {
		t.Fatalf("demoting the channel admin: %v", err)
	}

	after, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, adminDgraphUID)
	if err != nil {
		t.Fatalf("reading channel after demotion: %v", err)
	}
	if after == nil {
		t.Fatal("the channel read nothing after the demotion")
	}
	if after.IsAdmin != 0 {
		t.Fatalf("ch_is_admin is still %d after the demotion.\n"+
			"In an announcement channel (ch_post_policy admins_only) the demoted person "+
			"keeps posting for the rest of the cache TTL.", after.IsAdmin)
	}
}

// Removing someone from a channel, a project or a team takes it out of their
// search at once.
//
// Global search (and the AI's reach, and command delivery) is scoped by the
// channels, projects and teams in the person's graph profile, which the auth
// middleware reads on every request and keeps an hour (user:dgraph). No revoke
// path dropped it, so someone removed from a private channel kept finding what
// was posted there for up to an hour. GetActiveDgraphUserInfoByUUID is that
// read: what it lists after a removal is what search covers.
func TestRevokingMembershipTakesItOutOfTheirSearch(t *testing.T) {
	dg := wireStores(t)
	ctx := context.Background()

	memberUUID := uuid.NewString()
	channelUUID, projectUUID, teamUUID, teamProjectUUID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "dgraph.type": "User", "user_uuid": memberUUID, "user_name": "member",
			"user_channels": []map[string]any{{"uid": "_:ch"}},
			"user_projects": []map[string]any{{"uid": "_:proj"}, {"uid": "_:teamproj"}},
			"user_teams":    []map[string]any{{"uid": "_:team"}}},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channelUUID, "ch_name": "finance", "ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": projectUUID, "project_name": "audit",
			"project_members": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:teamproj", "dgraph.type": "Project", "project_uuid": teamProjectUUID, "project_name": "payroll",
			"project_members": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": teamUUID, "team_name": "people",
			"team_members":  []map[string]any{{"uid": "_:member"}},
			"team_projects": []map[string]any{{"uid": "_:teamproj"}}},
	})

	// What the member's next request reads, and caches.
	profile := func() (channels, projects, teams map[string]bool) {
		t.Helper()
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, memberUUID)
		if err != nil || u == nil {
			t.Fatalf("reading the member's profile: %v", err)
		}
		channels, projects, teams = map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, c := range u.Channels {
			channels[c.Uuid] = true
		}
		for _, p := range u.Projects {
			projects[p.Uuid] = true
		}
		for _, tm := range u.Teams {
			teams[tm.Uuid] = true
		}
		return
	}
	channels, projects, teams := profile()
	if !channels[channelUUID] || !projects[projectUUID] || !projects[teamProjectUUID] || !teams[teamUUID] {
		t.Fatalf("the fixture is wrong: the member isn't in everything to begin with (%v %v %v)", channels, projects, teams)
	}

	if err := channelBusiness.DeleteChannelMemberEdge(ctx, uids["ch"], uids["member"], memberUUID, channelUUID); err != nil {
		t.Fatalf("removing them from the channel: %v", err)
	}
	if channels, _, _ := profile(); channels[channelUUID] {
		t.Error("after removal from a private channel, search still covers it: their cached profile lists it")
	}

	if err := projectBusiness.RemoveMemberFromProject(ctx, uids["member"], memberUUID, uids["proj"], projectUUID); err != nil {
		t.Fatalf("removing them from the project: %v", err)
	}
	if _, projects, _ := profile(); projects[projectUUID] {
		t.Error("after removal from a project, search still covers it: their cached profile lists it")
	}

	team := &dgraphStruct.DgraphTeam{Uid: uids["team"], Uuid: teamUUID,
		Projects: []*dgraphStruct.DgraphProject{{Uid: uids["teamproj"], Uuid: teamProjectUUID}}}
	if err := teamBusiness.RemoveMemberFromTeam(ctx, team, uids["member"], memberUUID); err != nil {
		t.Fatalf("removing them from the team: %v", err)
	}
	if _, projects, teams := profile(); teams[teamUUID] || projects[teamProjectUUID] {
		t.Error("after removal from a team, search still covers it or its project: their cached profile lists them")
	}
}
