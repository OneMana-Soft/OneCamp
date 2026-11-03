//go:build integration
// +build integration

package integration_test

// Someone who isn't in a channel or a group asks to join its call: the green
// room said "Couldn't join: You don't have permission to do that", because the
// server's answer was a bare "Not Authorised" (QA F-22). It says why, in the
// person's words, and the green room shows that.
//
// A DM's call is with a person (V-126): one with an agent (a bot, by class) is
// refused through the same words, before any room or token, whatever a client
// offers, on either call route: the group route asked for a pair's id is the
// same room, and a group whose others are all agents has no one to call. A
// group with a person in it (an external guest too) still calls.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACallRefusalSaysWhy -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACallRefusalSaysWhy(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	sam, maya, jonas, channel, group := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.NewString()
	for name, id := range map[string]uuid.UUID{"sam": sam, "maya": maya, "jonas": jonas} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	atlas, guest := uuid.New(), uuid.New()
	pair, agentsOnly, withPerson, withGuest := helpers.GetGroupingId(sam.String(), atlas.String()), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_bot, created_at, updated_at)
		VALUES ($1, 'atlas@acme.test', 'atlas', 'Atlas', true, NOW(), NOW())`, atlas); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "jonas"},
		{"uid": "_:atlas", "dgraph.type": "User", "user_uuid": atlas.String(), "user_name": "atlas", "is_bot": true},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": []map[string]any{{"uid": "_:maya"}}},
		{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}},
		// An external guest: a person to call, though a DM's shown list leaves them out.
		{"uid": "_:guest", "dgraph.type": "User", "user_uuid": guest.String(), "user_name": "guest", "is_external": true},
		{"uid": "_:pair", "dgraph.type": "Dm", "dm_grouping_id": pair,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:atlas"}}},
		{"uid": "_:agentsOnly", "dgraph.type": "Dm", "dm_grouping_id": agentsOnly,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:atlas"}}},
		{"uid": "_:withPerson", "dgraph.type": "Dm", "dm_grouping_id": withPerson,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:maya"}, {"uid": "_:atlas"}}},
		{"uid": "_:withGuest", "dgraph.type": "Dm", "dm_grouping_id": withGuest,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:guest"}, {"uid": "_:atlas"}}},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})

	ask := func(handler http.HandlerFunc, body any) (code int, msg string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rctx, cancel := context.WithTimeout(asSam, 20*time.Second)
		defer cancel()
		// A route that panics refused nothing: said as its answer, so the
		// asking case fails on its own words and the others still run.
		defer func() {
			if p := recover(); p != nil {
				code, msg = 0, fmt.Sprintf("panic: %v", p)
			}
		}()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		var answer struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Msg
	}

	refusals := []struct {
		what string
		ask  func() (int, string)
		want string
	}{
		{"a channel's huddle", func() (int, string) {
			return ask(channelController.MakeVideoChannelCall, channelAdapter.InputMakeVideoChannelCall{ChannelUuid: channel.String()})
		}, "Only people in the channel can join its huddle. Join the channel first."},
		{"a group's call", func() (int, string) {
			return ask(chatController.MakeVideoCallForGroup, chatAdapter.InputMakeVideoChatCall{GrpUuid: group})
		}, "Only people in the group can join its call."},
	}
	for _, r := range refusals {
		if code, msg := r.ask(); code != http.StatusForbidden || msg != r.want {
			t.Errorf("%s, asked by someone not in it: %d %q, want 403 %q", r.what, code, msg, r.want)
		}
	}

	// V-126: Sam asks for a call in his DM with Atlas, an agent.
	const agent = "You can't call an agent. Send it a message instead."
	if code, msg := ask(chatController.MakeVideoCallForChat, chatAdapter.InputMakeVideoChatCall{UserUuid: atlas.String()}); code != http.StatusForbidden || msg != agent {
		t.Errorf("a DM's call with an agent: %d %q, want 403 %q", code, msg, agent)
	}
	// The group route: the same room by the pair's id, and a group of only agents (CR-178's F1).
	for what, id := range map[string]string{"Sam's DM with Atlas, by its pair id": pair, "a group of Sam and Atlas only": agentsOnly} {
		if code, msg := ask(chatController.MakeVideoCallForGroup, chatAdapter.InputMakeVideoChatCall{GrpUuid: id}); code != http.StatusForbidden || msg != agent {
			t.Errorf("the group route, %s: %d %q, want 403 %q", what, code, msg, agent)
		}
	}
	// A person to call, beside the agent: never refused.
	for what, id := range map[string]string{"a group with Maya and Atlas": withPerson, "a group with an external guest and Atlas": withGuest} {
		if code, msg := ask(chatController.MakeVideoCallForGroup, chatAdapter.InputMakeVideoChatCall{GrpUuid: id}); code == http.StatusForbidden {
			t.Errorf("the group route, %s: refused 403 %q, want a call", what, msg)
		}
	}
}
