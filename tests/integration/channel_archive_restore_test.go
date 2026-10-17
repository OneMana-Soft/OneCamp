//go:build integration
// +build integration

package integration_test

// An archived channel's history stays in its members' search, marked
// Archived, and its admins or a workspace admin can bring it back, against a
// real Postgres, Dgraph and OpenSearch.
//
// Archiving said "It can be restored", and nothing in the app could: no
// screen offered it, and the update route takes channel admins only. And
// archiving marked the channel's posts, replies and files deleted, out of
// everyone's search (SN-BUG-9).
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnArchivedChannelStaysFoundAndComesBack -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnArchivedChannelStaysFoundAndComesBack(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	client := opensearchInit.OpenSearchClient

	people := map[string]uuid.UUID{"mod": uuid.New(), "member": uuid.New(), "wsadmin": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		node := map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name}
		if name != "wsadmin" {
			node["user_channels"] = []map[string]any{{"uid": "_:launch"}}
		}
		nodes = append(nodes, node)
	}
	launch := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'launch', false, $2)`, launch, people["mod"]); err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch",
		"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z",
		"ch_members":    []map[string]any{{"uid": "_:mod"}, {"uid": "_:member"}},
		"ch_moderators": []map[string]any{{"uid": "_:mod"}}})
	uids := dg.Mutate(t, nodes)

	// A post in #launch, as the server writes it: its row (search drops a hit
	// whose content isn't there), and its entry in the index.
	post := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, launch, people["mod"]); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"post_id": post, "post_body": "the rollout checklist", "post_ch_id": launch.String(),
		"post_by_user_id": people["mod"].String(), "created_date": time.Now().Unix(), "deleted_date": nil})
	if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: "posts", DocumentID: post, Body: bytes.NewReader(raw),
		Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
		t.Fatal(err)
	}
	entry := func() map[string]any {
		_, _ = client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"posts"}})
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "posts", DocumentID: post})
		if err != nil {
			return nil
		}
		body, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(body, &got)
		return got.Source
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	// What the member finds searching for it, and whether it says Archived.
	memberFinds := func() (found, archived bool) {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, people["member"].String(), "member@example.test",
			[]*dgraphStruct.DgraphChannel{{Uuid: launch.String()}}, nil, nil, "rollout checklist")
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Page {
			if h.Post != nil && h.Post.Uuid == post {
				return true, h.Post.ChannelArchived != nil && *h.Post.ChannelArchived
			}
		}
		return false, false
	}

	if err := channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{ChannelUuid: launch.String(), ChannelName: "launch", ChannelArchived: true}, launch); err != nil {
		t.Fatalf("archiving #launch: %v", err)
	}
	eventually("the archived channel's post is marked", func() bool { e := entry(); return e != nil && e["channel_archived"] == true })
	if e := entry(); e["deleted_date"] != nil {
		t.Errorf("archiving marked the post deleted (%v); it stays, marked archived", e["deleted_date"])
	}
	if found, archived := memberFinds(); !found || !archived {
		t.Errorf("after archiving, the member finds the post %v, marked archived %v; want found and marked", found, archived)
	}

	// The archived list says who may restore it.
	for who, want := range map[string]uint8{"mod": 1, "member": 0} {
		list, _, err := channelDomain.GetUserArchivedChannelListWithLatestPost(ctx, uids[who], nil, 0, 20)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s's archived channels: %v (%v)", who, list, err)
		}
		if list[0].IsAdmin != want {
			t.Errorf("%s's archived #launch says ch_is_admin %d, want %d", who, list[0].IsAdmin, want)
		}
	}

	restore := func(who string, workspaceAdmin bool) (int, string) {
		t.Helper()
		var info userModels.UserInfo
		info.UserPostgresInfo.Id = people[who]
		info.UserPostgresInfo.IsAdmin = workspaceAdmin
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String()}
		rec := httptest.NewRecorder()
		channelController.RestoreChannel(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"channel_uuid":"`+launch.String()+`"}`)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, info)))
		return rec.Code, rec.Body.String()
	}
	if code, body := restore("member", false); code != http.StatusForbidden {
		t.Errorf("a member who isn't its admin restored #launch: %d %s", code, body)
	}
	// A workspace admin who isn't in the channel at all may.
	if code, body := restore("wsadmin", true); code != http.StatusOK {
		t.Fatalf("a workspace admin restoring #launch: %d %s", code, body)
	}
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, launch, uids["member"])
	if err != nil || ch == nil || helpers.IsSoftDeleted(ch.DeletedAt) {
		t.Errorf("#launch is still archived after the restore: %+v (%v)", ch, err)
	}
	eventually("the restored channel's post loses the mark", func() bool { e := entry(); return e != nil && e["channel_archived"] == nil })
	if found, archived := memberFinds(); !found || archived {
		t.Errorf("after the restore, the member finds the post %v, marked archived %v; want found, unmarked", found, archived)
	}
	if code, _ := restore("mod", false); code != http.StatusConflict {
		t.Errorf("restoring a live channel answered %d, want 409", code)
	}
}
