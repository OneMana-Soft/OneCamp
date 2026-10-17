//go:build integration
// +build integration

package integration_test

// The read a conversation's page stands on (a channel's info, the other
// person's profile, a group's members) says whether the conversation is
// there. Nothing by that id is a 404 in plain words, a private channel you
// aren't in a 403, and a graph that doesn't answer a 503 the app offers Try
// again for. They answered 400 for all of it, which the app can only read as
// "gone": an unknown channel drew a live page and "a connection problem", and
// a DM with an unknown person said "Failed to get user info err" (QA F-08 to
// F-10). A group that isn't there still answers with nothing in it, as a group
// made on a phone and not yet sent does.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAConversationsReadSaysWhetherItIsThere -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAConversationsReadSaysWhetherItIsThere(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	sam, maya, secret := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"sam": sam, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'leadership', true)`, secret); err != nil {
		t.Fatal(err)
	}
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:secret", "dgraph.type": "Channel", "ch_uuid": secret.String(), "ch_name": "leadership",
			"ch_private": true, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:maya"}}},
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

	type answer struct {
		code int
		msg  string
		data json.RawMessage
	}
	read := func(handler http.HandlerFunc, param, value string) answer {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add(param, value)
		rctx, cancel := context.WithTimeout(context.WithValue(asSam, chi.RouteCtxKey, rc), 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(rctx))
		var body struct {
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return answer{rec.Code, body.Msg, body.Data}
	}
	channel := func(id string) answer { return read(channelController.GetChannelBasicInfoByUUID, "channel_uuid", id) }
	person := func(id string) answer { return read(userController.GetProfileByUserId, "user_id", id) }
	group := func(id string) answer { return read(chatController.GetGroupDMParticipants, "grp_id", id) }

	t.Run("nothing by that id is a 404 in plain words", func(t *testing.T) {
		for what, got := range map[string]answer{
			"an unknown channel":          channel(uuid.NewString()),
			"a channel id that isn't one": channel("not-a-channel"),
		} {
			if got.code != http.StatusNotFound || got.msg != "Channel not found" {
				t.Errorf("%s: %d %q, want 404 \"Channel not found\"", what, got.code, got.msg)
			}
		}
		for what, got := range map[string]answer{
			"an unknown person":          person(uuid.NewString()),
			"a person id that isn't one": person("not-a-person"),
		} {
			if got.code != http.StatusNotFound || got.msg != "Person not found" {
				t.Errorf("%s: %d %q, want 404 \"Person not found\"", what, got.code, got.msg)
			}
		}
	})

	t.Run("a private channel you aren't in is a 403", func(t *testing.T) {
		if got := channel(secret.String()); got.code != http.StatusForbidden {
			t.Errorf("the private channel: %d %q, want 403", got.code, got.msg)
		}
	})

	t.Run("a group that isn't there answers with nothing in it", func(t *testing.T) {
		got := group(uuid.NewString())
		if got.code != http.StatusOK || (len(got.data) > 0 && string(got.data) != "null") {
			t.Errorf("an unknown group: %d %s, want 200 with no data", got.code, got.data)
		}
	})

	t.Run("a graph that doesn't answer is a 503, never 'gone'", func(t *testing.T) {
		dg.Close()
		for what, got := range map[string]answer{
			"a channel": channel(uuid.NewString()),
			"a person":  person(uuid.NewString()),
			"a group":   group(uuid.NewString()),
		} {
			if got.code != http.StatusServiceUnavailable {
				t.Errorf("%s while the graph doesn't answer: %d %q, want 503", what, got.code, got.msg)
			}
		}
	})
}
