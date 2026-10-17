//go:build integration
// +build integration

package integration_test

// A channel's creator was held in it for good: Details had Leave switched off
// for them, and the server refused to take them out, even by their own hand
// (QA F-46). They may leave, as anyone may, once someone else is an admin, so
// the channel keeps one; leaving takes their admin role with them. Nobody else
// removes the creator.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACreatorLeavesOnceTheChannelKeepsAnAdmin -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACreatorLeavesOnceTheChannelKeepsAnAdmin(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	maya, sam, kim, channel := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "sam": sam, "kim": kim} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:kim", "dgraph.type": "User", "user_uuid": kim.String(), "user_name": "kim"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z",
			"ch_created_by": map[string]any{"uid": "_:maya"},
			"ch_members":    []map[string]any{{"uid": "_:maya"}, {"uid": "_:sam"}, {"uid": "_:kim"}},
			"ch_moderators": []map[string]any{{"uid": "_:maya"}}},
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
	remove := func(by, who uuid.UUID) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(channelAdapter.InputChannelMemberInfo{ChannelUuid: channel.String(), UserUuid: who.String()})
		rctx, cancel := context.WithTimeout(as(by), 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		channelController.RemoveChannelMember(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		return rec.Code, rec.Body.String()
	}
	stepDown := func(by, who uuid.UUID) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(channelAdapter.InputChannelMemberInfo{ChannelUuid: channel.String(), UserUuid: who.String()})
		rctx, cancel := context.WithTimeout(as(by), 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		channelController.RemoveChannelModerator(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		return rec.Code, rec.Body.String()
	}
	inChannel := func() string {
		t.Helper()
		return string(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_members { user_uuid } ch_moderators { user_uuid } } }`))
	}
	admins := func() string {
		t.Helper()
		return string(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_moderators { user_uuid } } }`))
	}
	lastAdmin := func(code int, body string) bool {
		var answer struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &answer)
		return code == http.StatusConflict && answer.Code == "last_admin"
	}

	t.Run("nobody else removes the creator", func(t *testing.T) {
		if code, body := remove(sam, maya); code != http.StatusForbidden {
			t.Errorf("Sam removing the creator: %d %s, want 403", code, body)
		}
	})

	t.Run("the creator can't leave a channel that would keep no admin", func(t *testing.T) {
		code, body := remove(maya, maya)
		var answer struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &answer)
		if code != http.StatusConflict || answer.Code != "last_admin" {
			t.Errorf("the creator leaving as its only admin: %d %s, want 409 last_admin", code, body)
		}
		if !strings.Contains(inChannel(), maya.String()) {
			t.Errorf("the creator is out: %s", inChannel())
		}
	})

	t.Run("once someone else is an admin, the creator leaves, role and all", func(t *testing.T) {
		dg.Mutate(t, []map[string]any{{"uid": uids["ch"], "ch_moderators": []map[string]any{{"uid": uids["sam"]}}}})
		if code, body := remove(maya, maya); code != http.StatusOK {
			t.Fatalf("the creator leaving with Sam an admin: %d %s, want 200", code, body)
		}
		if got := inChannel(); strings.Contains(got, maya.String()) || !strings.Contains(got, sam.String()) {
			t.Errorf("after the creator left: %s, want Sam alone, as member and admin", got)
		}
	})

	// Since the creator may leave, any admin is the channel's last one in
	// turn: the rule that it keeps one holds for each of them, leaving or
	// stepping down.
	t.Run("the last admin can't leave or step down", func(t *testing.T) {
		if code, body := remove(sam, sam); !lastAdmin(code, body) {
			t.Errorf("Sam leaving as the only admin: %d %s, want 409 last_admin", code, body)
		}
		if code, body := stepDown(sam, sam); !lastAdmin(code, body) {
			t.Errorf("Sam stepping down as the only admin: %d %s, want 409 last_admin", code, body)
		}
		if got := admins(); !strings.Contains(got, sam.String()) {
			t.Errorf("the channel kept no admin: %s", got)
		}
	})

	t.Run("with another admin, an admin steps down, then leaves", func(t *testing.T) {
		dg.Mutate(t, []map[string]any{{"uid": uids["ch"], "ch_moderators": []map[string]any{{"uid": uids["kim"]}}}})
		if code, body := stepDown(sam, sam); code != http.StatusOK {
			t.Fatalf("Sam stepping down with Kim an admin: %d %s, want 200", code, body)
		}
		if code, body := remove(kim, kim); !lastAdmin(code, body) {
			t.Errorf("Kim leaving as the only admin now: %d %s, want 409 last_admin", code, body)
		}
		if code, body := remove(sam, sam); code != http.StatusOK {
			t.Errorf("Sam leaving, no longer an admin: %d %s, want 200", code, body)
		}
		if got := inChannel(); strings.Contains(got, sam.String()) || !strings.Contains(got, kim.String()) {
			t.Errorf("after Sam left: %s, want Kim, as member and admin", got)
		}
	})
}
