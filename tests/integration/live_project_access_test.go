//go:build integration
// +build integration

package integration_test

// Does live access follow a project's access, at the broker?
//
// An app's settings name a project's topic as they name a conversation's, and
// the broker never asks again about a topic a client holds. So a project made
// or joined after an app loaded never streamed into it (an open board took its
// topic up, nothing else did), and someone taken off a project, or off its
// team, kept its live messages in any app already open until it reconnected.
//
// These run the real join, make, removal and team removal paths against a
// real EMQX (live_access_test.go's stores and helpers).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestLive.*(Project|Team)' -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A live project's deletion time.
const liveProject = "0001-01-01T00:00:00Z"

// livePeople are accounts, one for each name.
func livePeople(t *testing.T, env *integration.Env, names ...string) map[string]uuid.UUID {
	t.Helper()
	out := map[string]uuid.UUID{}
	for _, n := range names {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", n+"-"+id.String()[:8], n); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
		out[n] = id
	}
	return out
}

// appOf is a client of someone's, as the web app connects one, on topics.
func appOf(t *testing.T, broker *integration.BrokerEnv, device string, who uuid.UUID, topics ...string) *integration.BrokerClient {
	t.Helper()
	c, _ := broker.Connect(t, "c_"+device+"_"+who.String()[:8]+"_ab12", who.String())
	c.Subscribe(t, topics...)
	return c
}

// heldUntilGone waits for a client to hold none of topics, and says what it
// holds then.
func heldUntilGone(t *testing.T, broker *integration.BrokerEnv, client string, topics ...string) []string {
	t.Helper()
	var held []string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		held = broker.Subscriptions(t, client)
		if !slices.ContainsFunc(topics, func(topic string) bool { return slices.Contains(held, topic) }) {
			break
		}
	}
	return held
}

// A project joined after the app loaded: the app is told, on the one topic it
// holds from the start, and the settings it fetches then name the project's
// topic.
func TestLiveJoiningAProjectTellsTheNewMembersApp(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()
	ana := livePeople(t, env, "ana")["ana"]
	project := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, ana); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch", "project_deleted_at": liveProject},
	})
	app := appOf(t, broker, "dev1", ana, helpers.GetMqttTopicForUserActivity(ana.String()))

	if err := projectBusiness.AddMemberToProject(ctx, project, uids["ana"], ana.String()); err != nil {
		t.Fatalf("Ana joins: %v", err)
	}
	ev, ok := conversationEvent(t, app, 10*time.Second)
	if !ok || ev.Kind != mqttStruct.ACCESS_KIND_PROJECT || ev.Id != project.String() || ev.Change != mqttStruct.CONVERSATION_ADDED {
		t.Fatalf("Ana's app wasn't told of the project she joined: %+v %v", ev, ok)
	}
	cfg, err := mqttBusiness.GetMqttConfig(ctx, ana.String(), false)
	if err != nil || !slices.Contains(cfg.Topics, helpers.GetMqttTopicForProjectMessage(project.String())) {
		t.Fatalf("the settings Ana's app fetches then don't name the project's topic: %v", err)
	}
}

// A project made: its maker's other open apps are told, as they are of a
// channel they make.
func TestLiveMakingAProjectTellsTheMakersApp(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()
	// The project is put in search as it's made: give the search somewhere to go.
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

	ana := livePeople(t, env, "ana")["ana"]
	team := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Ops', $2)`, team, ana); err != nil {
		t.Fatalf("seed the team: %v", err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Ops", "team_members": []map[string]any{{"uid": "_:ana"}}},
	})
	phone := appOf(t, broker, "dev2", ana, helpers.GetMqttTopicForUserActivity(ana.String()))

	made, err := projectBusiness.CreateProject(ctx, "Spring launch", uids["ana"], ana,
		&dgraphStruct.DgraphTeam{Uid: uids["team"], Uuid: team.String(), Name: "Ops"}, team,
		&dgraphStruct.DgraphUser{Uid: uids["ana"], Uuid: ana.String(), UserName: "ana"})
	if err != nil || made == nil {
		t.Fatalf("Ana makes a project: %v", err)
	}
	ev, ok := conversationEvent(t, phone, 10*time.Second)
	if !ok || ev.Kind != mqttStruct.ACCESS_KIND_PROJECT || ev.Id != made.Uuid || ev.Change != mqttStruct.CONVERSATION_ADDED {
		t.Fatalf("Ana's phone wasn't told of the project she made: %+v %v", ev, ok)
	}
}

// A bulk import's people aren't told of each project it puts them in, as
// they aren't of its channels.
func TestLiveABulkImportsPeopleArentToldOfItsProjects(t *testing.T) {
	env, dg, broker := liveStores(t)
	ana := livePeople(t, env, "ana")["ana"]
	project := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Imported', $2)`, project, ana); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Imported", "project_deleted_at": liveProject},
	})
	app := appOf(t, broker, "dev1", ana, helpers.GetMqttTopicForUserActivity(ana.String()))

	bulk := context.WithValue(context.Background(), helpers.BulkImportContextKey, true)
	if err := projectBusiness.AddMemberToProject(bulk, project, uids["ana"], ana.String()); err != nil {
		t.Fatalf("the import adds Ana: %v", err)
	}
	if ev, ok := conversationEvent(t, app, 2*time.Second); ok {
		t.Fatalf("Ana's app was told of a project an import put her in: %+v", ev)
	}
}

// Taken off a project: the app is told, the broker takes the project's topic
// off every client of theirs, and the project's next message reaches the
// members still in it and not them.
func TestLiveStreamOfAProjectEndsWhenSomeoneIsTakenOff(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()
	people := livePeople(t, env, "maya", "jonas")
	maya, jonas, project := people["maya"], people["jonas"], uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Budget', $2)`, project, jonas); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya", "user_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "jonas", "user_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Budget", "project_deleted_at": liveProject,
			"project_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}},
	})
	topic := helpers.GetMqttTopicForProjectMessage(project.String())
	mayaActivity := helpers.GetMqttTopicForUserActivity(maya.String())
	tab := appOf(t, broker, "dev1", maya, topic, mayaActivity)
	other := appOf(t, broker, "dev2", jonas, topic)

	if err := projectBusiness.RemoveMemberFromProject(ctx, uids["maya"], maya.String(), uids["p"], project.String()); err != nil {
		t.Fatalf("take Maya off the project: %v", err)
	}
	ev, ok := conversationEvent(t, tab, 10*time.Second)
	if !ok || ev.Kind != mqttStruct.ACCESS_KIND_PROJECT || ev.Id != project.String() || ev.Change != mqttStruct.CONVERSATION_DROPPED {
		t.Fatalf("Maya's app wasn't told it lost the project: %+v %v", ev, ok)
	}
	held := heldUntilGone(t, broker, tab.ID, topic)
	if slices.Contains(held, topic) {
		t.Fatalf("Maya's client still holds the project's topic: %v", held)
	}
	if !slices.Contains(held, mayaActivity) {
		t.Fatalf("Maya's client lost her own notifications too: %v", held)
	}

	drain(tab)
	publish(t, topic, `{"type":0,"data":{"task_name":"the Q4 numbers"}}`)
	if !receives(other, topic, "the Q4 numbers", 5*time.Second) {
		t.Fatal("Jonas, still in the project, didn't get its message")
	}
	if receives(tab, topic, "the Q4 numbers", 1500*time.Millisecond) {
		t.Fatal("Maya, taken off the project, still got its message")
	}
}

// Taken off a team, someone is off its projects: their app is told once, for
// the team, and the broker takes every one of the projects' topics off their
// clients.
func TestLiveTakingSomeoneOffATeamEndsItsProjectsStreams(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()
	people := livePeople(t, env, "maya", "jonas")
	maya, jonas := people["maya"], people["jonas"]
	team, launch, budget := uuid.New(), uuid.New(), uuid.New()
	both := []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}
	inBoth := []map[string]any{{"uid": "_:launch"}, {"uid": "_:budget"}}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya", "user_projects": inBoth, "user_teams": []map[string]any{{"uid": "_:team"}}},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "jonas", "user_projects": inBoth, "user_teams": []map[string]any{{"uid": "_:team"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Finance", "team_members": both, "team_projects": inBoth},
		{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch", "project_deleted_at": liveProject,
			"project_members": both, "project_team": map[string]any{"uid": "_:team"}},
		{"uid": "_:budget", "dgraph.type": "Project", "project_uuid": budget.String(), "project_name": "Budget", "project_deleted_at": liveProject,
			"project_members": both, "project_team": map[string]any{"uid": "_:team"}},
	})
	launchTopic, budgetTopic := helpers.GetMqttTopicForProjectMessage(launch.String()), helpers.GetMqttTopicForProjectMessage(budget.String())
	mayaActivity := helpers.GetMqttTopicForUserActivity(maya.String())
	tab := appOf(t, broker, "dev1", maya, launchTopic, budgetTopic, mayaActivity)
	other := appOf(t, broker, "dev2", jonas, launchTopic)

	finance := &dgraphStruct.DgraphTeam{Uid: uids["team"], Uuid: team.String(), Projects: []*dgraphStruct.DgraphProject{
		{Uid: uids["launch"], Uuid: launch.String()}, {Uid: uids["budget"], Uuid: budget.String()}}}
	if err := teamBusiness.RemoveMemberFromTeam(ctx, finance, uids["maya"], maya.String()); err != nil {
		t.Fatalf("take Maya off the team: %v", err)
	}
	ev, ok := conversationEvent(t, tab, 10*time.Second)
	if !ok || ev.Kind != mqttStruct.ACCESS_KIND_TEAM || ev.Id != team.String() || ev.Change != mqttStruct.CONVERSATION_DROPPED {
		t.Fatalf("Maya's app wasn't told it lost the team: %+v %v", ev, ok)
	}
	held := heldUntilGone(t, broker, tab.ID, launchTopic, budgetTopic)
	if slices.Contains(held, launchTopic) || slices.Contains(held, budgetTopic) {
		t.Fatalf("Maya's client still holds the team's projects' topics: %v", held)
	}
	if !slices.Contains(held, mayaActivity) {
		t.Fatalf("Maya's client lost her own notifications too: %v", held)
	}

	drain(tab)
	publish(t, launchTopic, `{"type":0,"data":{"task_name":"the launch date"}}`)
	if !receives(other, launchTopic, "the launch date", 5*time.Second) {
		t.Fatal("Jonas, still on the team, didn't get the project's message")
	}
	if receives(tab, launchTopic, "the launch date", 1500*time.Millisecond) {
		t.Fatal("Maya, taken off the team, still got its project's message")
	}
}
