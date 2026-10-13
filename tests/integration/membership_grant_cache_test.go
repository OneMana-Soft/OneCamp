//go:build integration
// +build integration

package integration_test

// Joining a channel, or being added to one, a project or a team, brings it
// into the person's search at once.
//
// Global search, the AI's reach and channel-scoped slash commands are scoped
// by the channels, projects and teams in the person's graph profile, which the
// auth middleware reads on every request and keeps an hour (user:dgraph). The
// revoke paths dropped it (TestRevokingMembershipTakesItOutOfTheirSearch); no
// grant path did, so someone who joined #design and searched a word posted
// there found nothing for up to an hour, and a channel they had just made was
// out of their own search. GetActiveDgraphUserInfoByUUID is that read: what it
// lists after a grant is what search covers.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGrantingMembershipBringsItIntoTheirSearch -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGrantingMembershipBringsItIntoTheirSearch(t *testing.T) {
	ctx := context.Background()
	// As wireStores, keeping the database: the grants write last-seen rows
	// and notification preferences against real users, channels and teams.
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	pg := env.PG

	// Making a channel, a project or a team writes its search entry in the
	// background; an index that answers "ok" keeps that from reaching a nil
	// client. What the index holds isn't the question here.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"_index":"x","_id":"x","result":"created","took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })

	member, owner := uuid.New(), uuid.New()
	channelUUID, projectUUID, teamUUID := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"member": member, "owner": owner} {
		if _, err := pg.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	// The rows the joins hang last-seen and notification preferences on.
	if _, err := pg.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'design', false, $2)`, channelUUID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'people', $2)`, teamUUID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'audit', $2, $3)`, projectUUID, teamUUID, owner); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "dgraph.type": "User", "user_uuid": member.String(), "user_name": "member"},
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channelUUID.String(), "ch_name": "design", "ch_private": false,
			"ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": []map[string]any{{"uid": "_:owner"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": teamUUID.String(), "team_name": "people",
			"team_members": []map[string]any{{"uid": "_:owner"}}},
		{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": projectUUID.String(), "project_name": "audit",
			"project_members": []map[string]any{{"uid": "_:owner"}}, "project_team": map[string]any{"uid": "_:team"}},
	})
	memberGraph := &dgraphStruct.DgraphUser{Uid: uids["member"], Uuid: member.String(), UserName: "member"}
	var memberInfo userModels.UserInfo
	memberInfo.UserPostgresInfo.Id = member
	memberInfo.UserDgraphInfo = *memberGraph

	// What the member's next request reads, and caches for an hour.
	profile := func() (channels, projects, teams map[string]bool) {
		t.Helper()
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, member.String())
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
	// Warm the cache the way any request does, holding none of them.
	if channels, projects, teams := profile(); len(channels)+len(projects)+len(teams) != 0 {
		t.Fatalf("the fixture is wrong: the member is in something to begin with (%v %v %v)", channels, projects, teams)
	}

	if err := channelBusiness.AddChannelMemberEdge(ctx, channelUUID, memberGraph, member); err != nil {
		t.Fatalf("joining #design: %v", err)
	}
	if channels, _, _ := profile(); !channels[channelUUID.String()] {
		t.Error("after joining #design, search still leaves it out: their cached profile doesn't list it")
	}

	err, made := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "launch-room"}, &memberInfo)
	if err != nil {
		t.Fatalf("making a channel: %v", err)
	}
	if channels, _, _ := profile(); !channels[made.String()] {
		t.Error("a channel they just made is out of their own search: their cached profile doesn't list it")
	}

	if err := projectBusiness.AddMemberToProject(ctx, projectUUID, uids["member"], member.String()); err != nil {
		t.Fatalf("adding them to the project: %v", err)
	}
	if _, projects, _ := profile(); !projects[projectUUID.String()] {
		t.Error("after being added to a project, search still leaves it out: their cached profile doesn't list it")
	}

	if err := teamBusiness.AddMemberToTeam(ctx, teamUUID, uids["member"], member.String()); err != nil {
		t.Fatalf("adding them to the team: %v", err)
	}
	if _, _, teams := profile(); !teams[teamUUID.String()] {
		t.Error("after being added to a team, search still leaves it out: their cached profile doesn't list it")
	}

	newTeam, err := teamBusiness.CreateTeam(ctx, "Field crew", &memberInfo)
	if err != nil || newTeam == nil {
		t.Fatalf("making a team: %v", err)
	}
	if _, _, teams := profile(); !teams[newTeam.Uuid] {
		t.Error("a team they just made is out of their own search: their cached profile doesn't list it")
	}
	// In the team seeded above, whose graph uid the test knows.
	team := &dgraphStruct.DgraphTeam{Uid: uids["team"], Uuid: teamUUID.String(), Name: "people"}
	newProject, err := projectBusiness.CreateProject(ctx, "Spring launch", uids["member"], member, team, teamUUID, memberGraph)
	if err != nil || newProject == nil {
		t.Fatalf("making a project: %v", err)
	}
	if _, projects, _ := profile(); !projects[newProject.Uuid] {
		t.Error("a project they just made is out of their own search: their cached profile doesn't list it")
	}
}
