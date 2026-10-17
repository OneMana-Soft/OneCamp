//go:build integration
// +build integration

package integration_test

// What the sidebar is sent, against a real Postgres and Dgraph.
//
//   - A private channel says so, and each channel carries the reader's
//     notification level: the sidebar compared levels with "none", which
//     doesn't exist ("block" does), from a payload that carried neither, so a
//     muted channel never looked muted and a private one showed "#" (SN-BUG-3).
//   - An archived channel leaves Starred as it leaves Channels (SN-BUG-7).
//   - A count that fails is left out and the rest is sent; the graph failing
//     answers 503. Any failure answered an empty 200, and the sidebar sat on
//     its skeleton (SN-BUG-6).
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheSidebarPayload -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheSidebarPayload(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'me@example.test', 'me', 'Me', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	general, secret, old := uuid.New(), uuid.New(), uuid.New()
	for ch, name := range map[uuid.UUID]string{general: "general", secret: "secret", old: "old"} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, ch, name, ch == secret); err != nil {
			t.Fatal(err)
		}
	}
	// #secret is muted (block), #general on everything.
	for ch, level := range map[uuid.UUID]string{general: "all", secret: "block"} {
		if _, err := env.PG.Exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`, me, ch, level); err != nil {
			t.Fatal(err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me",
			"user_channels":     []map[string]any{{"uid": "_:general"}, {"uid": "_:secret"}, {"uid": "_:old"}},
			"user_fav_channels": []map[string]any{{"uid": "_:general"}, {"uid": "_:old"}}},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_private": false, "ch_deleted_at": live},
		{"uid": "_:secret", "dgraph.type": "Channel", "ch_uuid": secret.String(), "ch_name": "secret", "ch_private": true, "ch_deleted_at": live},
		{"uid": "_:old", "dgraph.type": "Channel", "ch_uuid": old.String(), "ch_name": "old", "ch_private": false, "ch_deleted_at": "2026-10-01T00:00:00Z"},
	})

	sidebar := func(t *testing.T) *dgraphStruct.DgraphUser {
		t.Helper()
		u, err := userBusiness.GetDgraphUserInfoByUUIDForSidebarNav(ctx, me)
		if err != nil || u == nil {
			t.Fatalf("the sidebar: %v", err)
		}
		return u
	}

	t.Run("a private channel and a muted one say so", func(t *testing.T) {
		u := sidebar(t)
		got := map[string]*dgraphStruct.DgraphChannel{}
		for _, c := range u.Channels {
			got[c.Uuid] = c
		}
		if c := got[secret.String()]; c == nil || c.IsPrivate == nil || !*c.IsPrivate || c.NotificationType != "block" {
			t.Errorf("#secret is sent as %+v; want private and muted (block)", c)
		}
		if c := got[general.String()]; c == nil || (c.IsPrivate != nil && *c.IsPrivate) || c.NotificationType != "all" {
			t.Errorf("#general is sent as %+v; want public, on all", c)
		}
	})

	t.Run("an archived channel leaves Starred", func(t *testing.T) {
		u := sidebar(t)
		for _, c := range u.FavChannels {
			if c.Uuid == old.String() {
				t.Error("archived #old is still starred")
			}
		}
		if len(u.FavChannels) != 1 || u.FavChannels[0].NotificationType != "all" {
			t.Errorf("Starred holds %d channels; want #general, with its level", len(u.FavChannels))
		}
	})

	t.Run("a count that fails is left out, and the rest is sent", func(t *testing.T) {
		if _, err := env.PG.Exec(`ALTER TABLE posts RENAME TO posts_away`); err != nil {
			t.Fatal(err)
		}
		u, err := userBusiness.GetDgraphUserInfoByUUIDForSidebarNav(ctx, me)
		if _, rerr := env.PG.Exec(`ALTER TABLE posts_away RENAME TO posts`); rerr != nil {
			t.Fatal(rerr)
		}
		if err != nil || u == nil || len(u.Channels) != 2 {
			t.Fatalf("with the post count failing, the sidebar answered %v (%v); want its channels without counts", u, err)
		}
	})

	t.Run("the graph failing answers 503, never an empty 200", func(t *testing.T) {
		var info userModels.UserInfo
		info.UserPostgresInfo.Id = me
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: me.String()}
		as := context.WithValue(ctx, helpers.UserInfoContextKey, info)
		dg.Close()
		rec := httptest.NewRecorder()
		userController.GetDgraphUserInfoByUUIDForSidebarNav(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(as))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("with the graph down the sidebar answered %d %q; want 503", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["msg"] == nil {
			t.Errorf("the 503 says nothing: %q", rec.Body.String())
		}
	})
}
