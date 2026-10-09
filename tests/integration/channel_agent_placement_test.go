//go:build integration
// +build integration

package integration_test

// Who may put an agent in a channel, or take it out, from the channel's AI
// teammates panel: its owner or a workspace admin, either way; a channel's
// admins, to take it out (unless that leaves it in no channel, where it would
// answer everywhere); not just anyone in the channel, who could before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestChannelAgentPlacement -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestChannelAgentPlacement(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// The agent's member identity is indexed when it's first put in a
	// channel; give the index somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"result":"created"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]uuid.UUID{"owner": uuid.New(), "member": uuid.New(), "channelAdmin": uuid.New(), "workspaceAdmin": uuid.New()}
	var nodes []map[string]any
	var members []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
		members = append(members, map[string]any{"uid": "_:" + name})
	}
	general, elsewhere := uuid.NewString(), uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general, "ch_name": "general",
		"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": members,
		"ch_moderators": []map[string]any{{"uid": "_:channelAdmin"}}})
	uids := dg.Mutate(t, nodes)

	agent := uuid.New()
	scope := func(ids ...string) string {
		raw, _ := json.Marshal(map[string]any{"channel_ids": ids})
		return string(raw)
	}
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, scope) VALUES ($1, 'Release Captain', $2, 'mention', $3)`,
		agent, people["owner"], scope(elsewhere)); err != nil {
		t.Fatal(err)
	}
	channels := func() string {
		t.Helper()
		var raw string
		if err := env.PG.QueryRow(`SELECT scope::text FROM ai_agents WHERE id = $1`, agent).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var s struct {
			ChannelIDs []string `json:"channel_ids"`
		}
		_ = json.Unmarshal([]byte(raw), &s)
		names := map[string]string{general: "general", elsewhere: "elsewhere"}
		var out []string
		for _, id := range s.ChannelIDs {
			out = append(out, names[id])
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	set := func(who string, enabled bool) (int, string) {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.IsAdmin = who == "workspaceAdmin"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		raw, _ := json.Marshal(map[string]any{"channel_id": general, "agent_id": agent.String(), "enabled": enabled})
		rec := httptest.NewRecorder()
		channelController.SetChannelAITeammate(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u)))
		return rec.Code, rec.Body.String()
	}
	step := func(what, who string, enabled bool, wantCode int, wantChannels string) {
		t.Helper()
		code, body := set(who, enabled)
		if code != wantCode {
			t.Errorf("%s: %d %s, want %d", what, code, body, wantCode)
		}
		if got := channels(); got != wantChannels {
			t.Errorf("%s: the agent is in [%s], want [%s]", what, got, wantChannels)
		}
	}

	step("a member adds someone else's agent", "member", true, http.StatusForbidden, "elsewhere")
	step("a channel admin adds someone else's agent", "channelAdmin", true, http.StatusForbidden, "elsewhere")
	step("its owner adds it", "owner", true, http.StatusOK, "elsewhere,general")
	step("a member takes it out", "member", false, http.StatusForbidden, "elsewhere,general")
	step("a channel admin takes it out", "channelAdmin", false, http.StatusOK, "elsewhere")
	step("a workspace admin adds it", "workspaceAdmin", true, http.StatusOK, "elsewhere,general")

	// In this channel only: out of it, it would answer everywhere.
	if _, err := env.PG.Exec(`UPDATE ai_agents SET scope = $2 WHERE id = $1`, agent, scope(general)); err != nil {
		t.Fatal(err)
	}
	step("a channel admin takes it out of its only channel", "channelAdmin", false, http.StatusForbidden, "general")
	step("a workspace admin takes it out of its only channel", "workspaceAdmin", false, http.StatusOK, "")
}
