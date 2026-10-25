//go:build integration
// +build integration

package integration_test

// Does live access follow a project's access, at the broker?
//
// An app's settings name a project's topic as they name a conversation's, and
// the broker never asks again about a topic a client holds. So someone taken
// off a project, or off its team, kept its live messages in any app already
// open until it reconnected.
//
// These run the real removal and team removal paths against a real EMQX
// (live_access_test.go's stores and helpers).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestLive.*(Project|Team)' -v

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A live project's deletion time.
const liveProject = "0001-01-01T00:00:00Z"

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
		t.Errorf("Maya's app wasn't told it lost the project: %+v %v", ev, ok)
	}
	held := heldUntilGone(t, broker, tab.ID, topic)
	if slices.Contains(held, topic) {
		t.Errorf("Maya's client still holds the project's topic: %v", held)
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
		t.Errorf("Maya's app wasn't told it lost the team: %+v %v", ev, ok)
	}
	held := heldUntilGone(t, broker, tab.ID, launchTopic, budgetTopic)
	if slices.Contains(held, launchTopic) || slices.Contains(held, budgetTopic) {
		t.Errorf("Maya's client still holds the team's projects' topics: %v", held)
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
