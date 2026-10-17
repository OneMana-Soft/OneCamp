//go:build integration
// +build integration

package integration_test

// Someone who isn't in a channel or a group asks to join its call: the green
// room said "Couldn't join: You don't have permission to do that", because the
// server's answer was a bare "Not Authorised" (QA F-22). It says why, in the
// person's words, and the green room shows that.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACallRefusalSaysWhy -v

import (
	"bytes"
	"context"
	"encoding/json"
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
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "jonas"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": []map[string]any{{"uid": "_:maya"}}},
		{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}},
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

	ask := func(handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rctx, cancel := context.WithTimeout(asSam, 20*time.Second)
		defer cancel()
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
}
