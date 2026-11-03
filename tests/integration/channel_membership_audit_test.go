//go:build integration
// +build integration

package integration_test

// Who took whom out of a channel, or away from its managers, and when, is in
// the admin audit log, against real Postgres 12 and Dgraph. A membership and a
// manager's role are only edges, so a removal left nothing behind but the
// conversation index's left_at, with no who, and a rejoin clears that. Each row
// is written before the edge goes, by whoever asked, and a refused removal
// writes none.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEveryChannelMembershipChangeIsAudited

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestEveryChannelMembershipChangeIsAudited(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	maya, sam, kim, lee, channel := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "sam": sam, "kim": kim, "lee": lee} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel); err != nil {
		t.Fatal(err)
	}
	// Maya made the channel; she and Kim manage it.
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:kim", "dgraph.type": "User", "user_uuid": kim.String(), "user_name": "kim"},
		{"uid": "_:lee", "dgraph.type": "User", "user_uuid": lee.String(), "user_name": "lee"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z",
			"ch_created_by": map[string]any{"uid": "_:maya"},
			"ch_members":    []map[string]any{{"uid": "_:maya"}, {"uid": "_:sam"}, {"uid": "_:kim"}, {"uid": "_:lee"}},
			"ch_moderators": []map[string]any{{"uid": "_:maya"}, {"uid": "_:kim"}}},
	})

	as := func(id uuid.UUID) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", id, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	// call runs a channel route as by, about who, and answers its status.
	call := func(handler http.HandlerFunc, by, who uuid.UUID) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(channelAdapter.InputChannelMemberInfo{ChannelUuid: channel.String(), UserUuid: who.String()})
		rctx, cancel := context.WithTimeout(as(by), 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		return rec.Code, rec.Body.String()
	}
	inChannel := func() string {
		t.Helper()
		return string(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_members { user_uuid } } }`))
	}
	managers := func() string {
		t.Helper()
		return string(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_moderators { user_uuid } } }`))
	}
	// check holds the one row of action to who asked (a person), the channel,
	// who it was about, and when: written within this run.
	check := func(t *testing.T, action string, by, who uuid.UUID) {
		t.Helper()
		if kinds := audited(t, env.PG, action, 1); kinds[0] != "human" {
			t.Errorf("%s is by a %q, want a human", action, kinds[0])
		}
		var actor, channelUUID, channelName, person string
		var recent bool
		if err := env.PG.QueryRow(`SELECT COALESCE(actor_id::text, ''), COALESCE(metadata->>'channel_uuid', ''),
			COALESCE(metadata->>'channel_name', ''), COALESCE(metadata->>'user_uuid', ''),
			created_at > NOW() - interval '10 minutes' FROM admin_audit_log WHERE action = $1`, action).
			Scan(&actor, &channelUUID, &channelName, &person, &recent); err != nil {
			t.Fatal(err)
		}
		if actor != by.String() || channelUUID != channel.String() || channelName != "launch" || person != who.String() || !recent {
			t.Errorf("%s names actor %q, channel %q %q, person %q, recent %v; want %s, %s \"launch\", %s, true",
				action, actor, channelUUID, channelName, person, recent, by, channel, who)
		}
	}
	channelRows := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action LIKE 'channel.%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a manager takes someone out", func(t *testing.T) {
		if code, body := call(channelController.RemoveChannelMember, maya, lee); code != http.StatusOK {
			t.Fatalf("Maya taking Lee out: %d %s, want 200", code, body)
		}
		if strings.Contains(inChannel(), lee.String()) {
			t.Errorf("Lee is still in: %s", inChannel())
		}
		check(t, "channel.member.remove", maya, lee)
	})
	// A change only managers make is refused to anyone else with 403, the
	// one answer for want of a role, and a refusal writes no row.
	t.Run("a refused change answers 403 and writes no row", func(t *testing.T) {
		if code, body := call(channelController.RemoveChannelMember, sam, maya); code != http.StatusForbidden {
			t.Errorf("Sam taking the creator out: %d %s, want 403", code, body)
		}
		if code, body := call(channelController.RemoveChannelMember, sam, kim); code != http.StatusForbidden {
			t.Errorf("Sam, no manager, taking Kim out: %d %s, want 403", code, body)
		}
		if code, body := call(channelController.RemoveChannelModerator, sam, kim); code != http.StatusForbidden {
			t.Errorf("Sam, no manager, taking Kim's role: %d %s, want 403", code, body)
		}
		if code, body := call(channelController.AddChannelModerators, sam, kim); code != http.StatusForbidden {
			t.Errorf("Sam, no manager, making Kim a manager: %d %s, want 403", code, body)
		}
		raw, _ := json.Marshal(channelAdapter.UpdateChannelInfo{ChannelUuid: channel.String(), ChannelName: "renamed"})
		rec := httptest.NewRecorder()
		channelController.UpdateChannelInfo(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(sam)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("Sam, no manager, renaming the channel: %d %s, want 403", rec.Code, rec.Body.String())
		}
		if n := channelRows(); n != 1 {
			t.Errorf("%d channel rows after the refusals, want the 1 before them", n)
		}
	})
	t.Run("a manager takes away another's role", func(t *testing.T) {
		if code, body := call(channelController.RemoveChannelModerator, maya, kim); code != http.StatusOK {
			t.Fatalf("Maya taking Kim's manager role: %d %s, want 200", code, body)
		}
		if got := managers(); strings.Contains(got, kim.String()) || !strings.Contains(inChannel(), kim.String()) {
			t.Errorf("Kim should be a member, no longer a manager: managers %s, members %s", got, inChannel())
		}
		check(t, "channel.manager.remove", maya, kim)
	})
	t.Run("a manager steps down", func(t *testing.T) {
		dg.Mutate(t, []map[string]any{{"uid": uids["ch"], "ch_moderators": []map[string]any{{"uid": uids["sam"]}}}})
		if code, body := call(channelController.RemoveChannelModerator, sam, sam); code != http.StatusOK {
			t.Fatalf("Sam stepping down: %d %s, want 200", code, body)
		}
		check(t, "channel.manager.step_down", sam, sam)
	})
	t.Run("someone leaves", func(t *testing.T) {
		if code, body := call(channelController.RemoveChannelMember, sam, sam); code != http.StatusOK {
			t.Fatalf("Sam leaving: %d %s, want 200", code, body)
		}
		check(t, "channel.member.leave", sam, sam)
		if n := channelRows(); n != 4 {
			t.Errorf("%d channel rows, want 4: one per change", n)
		}
	})
}
