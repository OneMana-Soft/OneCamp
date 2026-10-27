//go:build integration
// +build integration

package integration_test

// Which channels a person is in is theirs. Someone else's profile, and the
// people a channel's details offer to add, never carry anyone's channels; and
// that offer is read only by someone who can read the channel (a private one's
// members), so what it leaves out never names a private channel's members to
// anyone else.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestAPersonsChannelsAreTheirOwn$' -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAPersonsChannelsAreTheirOwn(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	// A profile's read asks the call server whether the two people are in a
	// call (business/User GetDmCallStatus); with no server it panics.
	integration.SetupLiveKit(t)
	dg := integration.SetupDgraph(t)

	// ana and cy are in a private channel and a public one; dee only in the
	// public one; ben in neither.
	private, public := uuid.NewString(), uuid.NewString()
	in := map[string][]string{"ana": {"private", "public"}, "cy": {"private", "public"}, "dee": {"public"}, "ben": nil}
	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"ana", "ben", "cy", "dee"} {
		ids[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			ids[n], n+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		var channels []map[string]any
		for _, c := range in[n] {
			channels = append(channels, map[string]any{"uid": "_:" + c})
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": ids[n].String(), "user_name": n,
			"user_full_name": n, "user_email_id": n + "@example.test", "user_deleted_at": "0001-01-01T00:00:00Z", "user_channels": channels})
	}
	members := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:private", "dgraph.type": "Channel", "ch_uuid": private, "ch_name": "board", "ch_private": true,
			"ch_members": members("ana", "cy"), "ch_moderators": members("ana")},
		map[string]any{"uid": "_:public", "dgraph.type": "Channel", "ch_uuid": public, "ch_name": "general", "ch_private": false,
			"ch_members": members("ana", "cy", "dee"), "ch_moderators": members("ana")})
	uids := dg.Mutate(t, nodes)

	call := func(who string, h http.HandlerFunc, key, value string) (int, string) {
		t.Helper()
		info := userModels.UserInfo{}
		info.UserPostgresInfo.Id = ids[who]
		info.UserPostgresInfo.EmailID = who + "@example.test"
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: ids[who].String()}
		rc := chi.NewRouteContext()
		rc.URLParams.Add(key, value)
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, info), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(c))
		return rec.Code, rec.Body.String()
	}
	namesNoChannel := func(who, what, body string) {
		t.Helper()
		for _, c := range []string{private, public} {
			if strings.Contains(body, c) {
				t.Errorf("%s: %s names a channel someone is in (%s)", who, what, c)
			}
		}
	}

	// ana's profile, as ben reads it, says who she is, not where she talks.
	code, body := call("ben", userController.GetProfileByUserId, "user_id", ids["ana"].String())
	if code != http.StatusOK || !strings.Contains(body, ids["ana"].String()) {
		t.Fatalf("ben reading ana's profile: %d %s", code, body)
	}
	namesNoChannel("ben", "ana's profile", body)

	// Her own profile still names both of her channels.
	code, body = call("ana", userController.GetProfileByUserId, "user_id", ids["ana"].String())
	if code != http.StatusOK || !strings.Contains(body, private) || !strings.Contains(body, public) {
		t.Errorf("ana reading her own profile: %d %.300s, want both her channels named", code, body)
	}

	// The private channel's people to add are for its members: to ben, as to
	// any channel that isn't there, there is no such list.
	for _, ch := range []string{private, uuid.NewString()} {
		if code, body := call("ben", userController.UsersListNotBelongToChannelId, "channel_id", ch); code != http.StatusNotFound {
			t.Errorf("ben, outside the private channel, listed who isn't in %s: %d %.200s", ch, code, body)
		}
	}

	// cy, its member, is offered ben and dee, and nobody's channels.
	code, body = call("cy", userController.UsersListNotBelongToChannelId, "channel_id", private)
	if code != http.StatusOK || !strings.Contains(body, ids["ben"].String()) || !strings.Contains(body, ids["dee"].String()) {
		t.Fatalf("cy, in the private channel, reading who she could add: %d %.300s", code, body)
	}
	if strings.Contains(body, ids["ana"].String()) {
		t.Errorf("cy is offered ana, who is in the channel already: %.300s", body)
	}
	namesNoChannel("cy", "the people to add", body)

	// A public channel's people to add are anyone's to read, and name nobody's
	// channels either.
	code, body = call("ben", userController.UsersListNotBelongToChannelId, "channel_id", public)
	if code != http.StatusOK || !strings.Contains(body, ids["ben"].String()) {
		t.Fatalf("ben reading who isn't in the public channel: %d %.300s", code, body)
	}
	namesNoChannel("ben", "the public channel's people to add", body)
}
