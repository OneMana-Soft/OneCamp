//go:build integration
// +build integration

package integration_test

// Can a person's client subscribe only to what that person can read?
//
// The broker asks business/MqttAccess before every subscription. Its unit tests
// decide against fakes; this runs the real lookups: the person's channels and
// conversations from the graph (read once, kept, read again when the client
// fetches its settings after joining or leaving), a private doc's grants, and
// the active-user check in Postgres.
//
// Run: go test -tags=integration ./tests/integration/ -run TestMqttAccess -v

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	mqttAccess "github.com/akashc777/OneCamp/business/MqttAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestMqttAccessFollowsWhatAPersonCanRead(t *testing.T) {
	t.Setenv("JWT_SECRET", "mqtt-access-integration-secret")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	member, admin := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{member, admin} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", "p-"+id.String()[:8], "p"); err != nil {
			t.Fatalf("seed a person: %v", err)
		}
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO admin_users (email_id) VALUES ($1)`, admin.String()+"@example.test"); err != nil {
		t.Fatalf("seed an admin: %v", err)
	}

	inChannel, otherChannel := uuid.NewString(), uuid.NewString()
	grouping := "0123456789abcdef0123456789abcdef"
	privateDoc, sharedDoc, openDoc := uuid.NewString(), uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "user_uuid": member.String(), "user_name": "member",
			"user_channels": []map[string]any{{"uid": "_:in"}},
			"user_dms":      []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:owner", "user_uuid": uuid.NewString(), "user_name": "owner"},
		{"uid": "_:in", "ch_uuid": inChannel, "ch_name": "general",
			"ch_members": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:out", "ch_uuid": otherChannel, "ch_name": "finance", "ch_private": true},
		{"uid": "_:dm", "dm_grouping_id": grouping},
		{"uid": "_:private", "doc_uuid": privateDoc, "doc_title": "plans", "doc_private": true,
			"doc_created_by": map[string]any{"uid": "_:owner"}},
		{"uid": "_:shared", "doc_uuid": sharedDoc, "doc_title": "notes", "doc_private": true,
			"doc_created_by":    map[string]any{"uid": "_:owner"},
			"doc_reading_users": []map[string]any{{"uid": "_:member"}}},
		{"uid": "_:open", "doc_uuid": openDoc, "doc_title": "handbook", "doc_private": false,
			"doc_created_by": map[string]any{"uid": "_:owner"}},
	})

	ask := func(who uuid.UUID, topic string) bool {
		return mqttAccess.Allowed(ctx, mqttAccess.Request{Username: "user_" + who.String(), Topic: topic, Action: "subscribe"})
	}
	inMsg, inTyping := helpers.GetMqttTopicForChannel(inChannel)
	outMsg, outTyping := helpers.GetMqttTopicForChannel(otherChannel)
	dmMsg, dmTyping := helpers.GetMqttTopicForDm(grouping)

	for _, c := range []struct {
		name  string
		who   uuid.UUID
		topic string
		want  bool
	}{
		{"their channel", member, inMsg, true},
		{"typing in their channel", member, inTyping, true},
		{"a private channel they're not in", member, outMsg, false},
		{"typing in it", member, outTyping, false},
		{"their conversation", member, dmMsg, true},
		{"typing in their conversation", member, dmTyping, true},
		{"a private doc", member, helpers.GetMqttTopicForDoc(privateDoc), false},
		{"a private doc shared with them", member, helpers.GetMqttTopicForDoc(sharedDoc), true},
		{"a doc that isn't private", member, helpers.GetMqttTopicForDoc(openDoc), true},
		{"a doc that doesn't exist", member, helpers.GetMqttTopicForDoc(uuid.NewString()), false},
		{"their notifications", member, helpers.GetMqttTopicForUserActivity(member.String()), true},
		{"someone else's", member, helpers.GetMqttTopicForUserActivity(admin.String()), false},
		{"the admin broadcast", member, helpers.GetMqttTopicForAdminBroadcast(), false},
		{"the admin broadcast, as an admin", admin, helpers.GetMqttTopicForAdminBroadcast(), true},
		{"every channel", member, "message/#", false},
		{"someone with no account", uuid.New(), helpers.GetPublicUsersStatusTopic(), false},
	} {
		if got := ask(c.who, c.topic); got != c.want {
			t.Errorf("%s: allowed=%v, want %v", c.name, got, c.want)
		}
	}

	// Joining a channel: the client fetches its settings, which hand it the
	// channel's topics, then subscribes to them.
	dg.Mutate(t, map[string]any{"uid": uids["member"], "user_channels": []map[string]any{{"uid": uids["out"]}}})
	cfg, err := mqttBusiness.GetMqttConfig(ctx, member.String(), false)
	if err != nil {
		t.Fatalf("settings after joining: %v", err)
	}
	if !slices.Contains(cfg.Topics, outMsg) {
		t.Fatalf("the settings after joining don't hand out the channel's topic: %v", cfg.Topics)
	}
	if !ask(member, outMsg) || !ask(member, outTyping) {
		t.Error("a channel just joined can't be subscribed to")
	}

	// Leaving it: the same, the other way.
	raw, _ := json.Marshal(map[string]any{"uid": uids["member"], "user_channels": []map[string]any{{"uid": uids["out"]}}})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DeleteJson: raw, CommitNow: true}); err != nil {
		t.Fatalf("leave the channel: %v", err)
	}
	if _, err := mqttBusiness.GetMqttConfig(ctx, member.String(), false); err != nil {
		t.Fatalf("settings after leaving: %v", err)
	}
	if ask(member, outMsg) {
		t.Error("a channel just left can still be subscribed to")
	}
	if !ask(member, inMsg) {
		t.Error("leaving one channel lost another")
	}
}
