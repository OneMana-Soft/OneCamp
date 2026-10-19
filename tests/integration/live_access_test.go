//go:build integration
// +build integration

package integration_test

// Does live access follow access, at the broker?
//
// The broker asks the server about each subscription a client makes, and
// never again about one it holds. So a person removed from a private channel
// kept its live messages in any app already open, and a client resuming its
// session (the web app keeps one for an hour) got the subscription back
// unasked; a deactivated person kept everything; a conversation started or
// joined after an app loaded never streamed into it; and a group that took
// someone in moved to a new id (its members' hash) that no open app held.
//
// These run the real removal, deactivation, join and add paths against a
// real EMQX, at the compose files' version, and read the broker's own state.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestLive' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// liveStores brings up what the membership paths write to, and the broker.
func liveStores(t *testing.T) (*integration.Env, *integration.DgraphEnv, *integration.BrokerEnv) {
	t.Helper()
	t.Setenv("JWT_SECRET", "live-access-integration-secret")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	return env, dg, integration.SetupBroker(t)
}

// conversationEvent waits for a conversation access event on a client.
func conversationEvent(t *testing.T, c *integration.BrokerClient, within time.Duration) (mqttStruct.MqttConversationAccess, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		m, ok := c.Next(time.Until(deadline))
		if !ok {
			break
		}
		var msg struct {
			Type int8                              `json:"type"`
			Data mqttStruct.MqttConversationAccess `json:"data"`
		}
		if json.Unmarshal(m.Payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_CONVERSATION_ACCESS {
			return msg.Data, true
		}
	}
	return mqttStruct.MqttConversationAccess{}, false
}

// receives reports whether a client gets a message on topic carrying marker
// within wait.
func receives(c *integration.BrokerClient, topic, marker string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m, ok := c.Next(time.Until(deadline))
		if !ok {
			return false
		}
		if m.Topic == topic && strings.Contains(string(m.Payload), marker) {
			return true
		}
	}
	return false
}

// drain empties what a client has received so far.
func drain(c *integration.BrokerClient) {
	for {
		if _, ok := c.Next(300 * time.Millisecond); !ok {
			return
		}
	}
}

func publish(t *testing.T, topic, payload string) {
	t.Helper()
	tok := mqttInit.MqttClient.Publish(topic, 1, false, payload)
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("publish: %v", tok.Error())
	}
}

func TestLiveStreamOfAPrivateChannelEndsWhenSomeoneIsRemoved(t *testing.T) {
	_, dg, broker := liveStores(t)
	ctx := context.Background()

	maya, jonas := uuid.NewString(), uuid.NewString()
	channel := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "user_uuid": maya, "user_name": "maya", "user_channels": []map[string]any{{"uid": "_:ch"}}},
		{"uid": "_:jonas", "user_uuid": jonas, "user_name": "jonas", "user_channels": []map[string]any{{"uid": "_:ch"}}},
		{"uid": "_:ch", "ch_uuid": channel, "ch_name": "leadership", "ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}},
	})
	msg, typing := helpers.GetMqttTopicForChannel(channel)
	mayaActivity := helpers.GetMqttTopicForUserActivity(maya)

	// Maya's tab and Jonas's, as the web app connects them.
	tab, _ := broker.Connect(t, "c_dev1_"+maya[:8]+"_ab12", maya)
	tab.Subscribe(t, msg, typing, mayaActivity)
	other, _ := broker.Connect(t, "c_dev2_"+jonas[:8]+"_cd34", jonas)
	other.Subscribe(t, msg)

	// An admin removes her.
	if err := channelBusiness.DeleteChannelMemberEdge(ctx, uids["ch"], uids["maya"], maya, channel); err != nil {
		t.Fatalf("remove Maya: %v", err)
	}

	// Her app is told...
	ev, ok := conversationEvent(t, tab, 10*time.Second)
	if !ok || ev.Kind != mqttStruct.CONVERSATION_KIND_CHANNEL || ev.Id != channel || ev.Change != mqttStruct.CONVERSATION_DROPPED {
		t.Fatalf("Maya's app wasn't told it lost the channel: %+v %v", ev, ok)
	}
	// ...and the broker takes the channel's topics off her client.
	var held []string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		held = broker.Subscriptions(t, tab.ID)
		if !slices.Contains(held, msg) && !slices.Contains(held, typing) {
			break
		}
	}
	if slices.Contains(held, msg) || slices.Contains(held, typing) {
		t.Fatalf("Maya's client still holds the channel's topics: %v", held)
	}
	if !slices.Contains(held, mayaActivity) {
		t.Fatalf("Maya's client lost her own notifications too: %v", held)
	}

	// The next post reaches Jonas and not her. (What reached her before the
	// cut, the channel's own "member removed", is set aside first.)
	drain(tab)
	publish(t, msg, `{"type":0,"data":{"post_text":"the numbers for Q4"}}`)
	if !receives(other, msg, "the numbers for Q4", 5*time.Second) {
		t.Fatal("Jonas, still a member, didn't get the post")
	}
	if receives(tab, msg, "the numbers for Q4", 1500*time.Millisecond) {
		t.Fatal("Maya, removed, still got the channel's post")
	}

	// Nor when her app resumes its session after a drop: the broker gives a
	// resumed session its subscriptions back without asking about them.
	tab.Disconnect()
	publish(t, msg, `{"type":0,"data":{"post_text":"while you were away"}}`)
	back, present := broker.Connect(t, tab.ID, maya)
	if !present {
		t.Fatal("the fixture is wrong: the broker kept no session to resume, so resuming proves nothing")
	}
	if receives(back, msg, "while you were away", 1500*time.Millisecond) {
		t.Fatal("Maya's resumed session got the channel's post")
	}
}

func TestLiveStreamEndsForSomeoneDeactivated(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()

	sam := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, sam, sam.String()+"@example.test"); err != nil {
		t.Fatalf("seed Sam: %v", err)
	}
	dg.Mutate(t, map[string]any{"uid": "_:sam", "user_uuid": sam.String(), "user_name": "sam"})
	msg, _ := helpers.GetMqttTopicForChannel(uuid.NewString())

	laptop, _ := broker.Connect(t, "c_dev1_"+sam.String()[:8]+"_ab12", sam.String())
	laptop.Subscribe(t, msg)
	phone, _ := broker.Connect(t, "c_dev2_"+sam.String()[:8]+"_cd34", sam.String())
	phone.Subscribe(t, msg)

	if err := userBusiness.DeactivateUser(ctx, sam); err != nil {
		t.Fatalf("deactivate Sam: %v", err)
	}
	if !laptop.Lost(15*time.Second) || !phone.Lost(5*time.Second) {
		t.Fatal("a deactivated person's clients are still connected")
	}
	// The session went with them: a client coming back starts afresh.
	back, present := broker.Connect(t, laptop.ID, sam.String())
	if present {
		t.Fatal("a deactivated person's session survived, subscriptions and all")
	}
	publish(t, msg, `{"type":0,"data":{"post_text":"after"}}`)
	if receives(back, msg, "after", 1500*time.Millisecond) {
		t.Fatal("a deactivated person still gets the channel's messages")
	}
}

// A channel joined after the app loaded: the app is told, on the one topic
// it holds from the start, and fetches its topics again.
func TestLiveJoiningAChannelTellsTheNewMembersApp(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()

	ana := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'ana', 'Ana', NOW(), NOW())`, ana, ana.String()+"@example.test"); err != nil {
		t.Fatalf("seed Ana: %v", err)
	}
	channel := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel); err != nil {
		t.Fatalf("seed the channel: %v", err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:ch", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": false},
	})
	app, _ := broker.Connect(t, "c_dev1_"+ana.String()[:8]+"_ab12", ana.String())
	app.Subscribe(t, helpers.GetMqttTopicForUserActivity(ana.String()))

	user, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ana.String())
	if err != nil || user == nil || user.Uid != uids["ana"] {
		t.Fatalf("read Ana: %+v %v", user, err)
	}
	if err := channelBusiness.AddChannelMemberEdge(ctx, channel, user, ana); err != nil {
		t.Fatalf("Ana joins: %v", err)
	}
	ev, ok := conversationEvent(t, app, 10*time.Second)
	if !ok || ev.Id != channel.String() || ev.Change != mqttStruct.CONVERSATION_ADDED || ev.Kind != mqttStruct.CONVERSATION_KIND_CHANNEL {
		t.Fatalf("Ana's app wasn't told of the channel she joined: %+v %v", ev, ok)
	}
}

// Adding someone to a group opens the conversation of the new set of people
// (D-S1): every one of them, the person added included, is told of it on
// their own topic and subscribes without reconnecting, and nobody loses the
// group they were in.
func TestLiveAddingToAGroupOpensTheNewSetsConversationForEveryone(t *testing.T) {
	env, dg, broker := liveStores(t)
	ctx := context.Background()

	// The new group's first line is searched for nobody, but a copy would be:
	// give the search somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"timed_out":false,"total":0,"updated":0,"deleted":0,"batches":0,"version_conflicts":0,"noops":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]string{"maya": uuid.NewString(), "jonas": uuid.NewString(), "sam": uuid.NewString(), "ana": uuid.NewString()}
	// Everyone in a group has an account: the new group's settings and read
	// marks name the account.
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id+"@example.test", name+"-"+id[:8], name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	before, err := helpers.GenerateGroupID([]string{people["maya"], people["jonas"], people["sam"]})
	if err != nil {
		t.Fatal(err)
	}
	after, err := helpers.GenerateGroupID([]string{people["maya"], people["jonas"], people["sam"], people["ana"]})
	if err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "user_uuid": people["maya"], "user_name": "maya", "user_dms": []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:jonas", "user_uuid": people["jonas"], "user_name": "jonas", "user_dms": []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:sam", "user_uuid": people["sam"], "user_name": "sam", "user_dms": []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:ana", "user_uuid": people["ana"], "user_name": "ana"},
		{"uid": "_:dm", "dm_grouping_id": before,
			"dm_participants": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}, {"uid": "_:sam"}}},
	})

	_ = uids

	apps := map[string]*integration.BrokerClient{}
	for name, id := range people {
		app, _ := broker.Connect(t, "c_dev_"+id[:8]+"_"+name, id)
		app.Subscribe(t, helpers.GetMqttTopicForUserActivity(id))
		apps[name] = app
	}

	maya, err := userDomain.GetUserByUUID(ctx, uuid.MustParse(people["maya"]))
	if err != nil || maya == nil {
		t.Fatalf("read Maya: %v", err)
	}
	mayaNode, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["maya"])
	if err != nil || mayaNode == nil {
		t.Fatalf("read Maya's node: %v", err)
	}
	who := &userModels.UserInfo{UserPostgresInfo: *maya, UserDgraphInfo: *mayaNode}
	opened, err := conversationBusiness.AddPeople(ctx, who, conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: before},
		[]string{people["ana"]}, conversationBusiness.ShareNone)
	if err != nil || opened.ID != after {
		t.Fatalf("Maya adds Ana: %+v %v", opened, err)
	}
	for name, app := range apps {
		ev, ok := conversationEvent(t, app, 10*time.Second)
		if !ok || ev.Kind != mqttStruct.CONVERSATION_KIND_GROUP || ev.Id != after || ev.Change != mqttStruct.CONVERSATION_ADDED {
			t.Errorf("%s's app wasn't told of the new group: %+v %v", name, ev, ok)
		}
	}
	// Nobody lost the group they were in: its people are as they were.
	if got := string(dg.Query(t, `{ q(func: eq(dm_grouping_id, "`+before+`")) { n: count(dm_participants) } }`)); !strings.Contains(got, `"n":3`) {
		t.Errorf("the group people were added from: %s", got)
	}
}
