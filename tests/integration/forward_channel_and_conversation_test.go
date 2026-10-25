//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A forward to a channel and to conversations at once writes the channel's post and
// each conversation's message in one go. The two inserts went to Postgres as one
// parameterised statement, which it refuses ("cannot insert multiple commands into a
// prepared statement"), so such a forward failed with no files at all.
func TestAForwardToAChannelAndConversationsReachesEveryOne(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	sam, alex, kim := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"sam": sam, "alex": alex, "kim": kim} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	general, open, source := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"general": general, "open": open} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, source, general, sam); err != nil {
		t.Fatal(err)
	}
	crew, err := helpers.GenerateGroupID([]string{sam.String(), alex.String(), kim.String()})
	if err != nil {
		t.Fatal(err)
	}
	live := "0001-01-01T00:00:00Z"
	person := func(name string, id uuid.UUID) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live}
	}
	uids := dg.Mutate(t, []map[string]any{
		person("sam", sam), person("alex", alex), person("kim", kim),
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}},
			"ch_posts": []map[string]any{{"uid": "_:source", "dgraph.type": "Post", "post_uuid": source.String(),
				"post_text": "<p>ship it</p>", "post_by": map[string]any{"uid": "_:sam"}, "post_channel": map[string]any{"uid": "_:general"},
				"post_created_at": "2026-10-01T10:00:00Z", "post_deleted_at": live}}},
		{"uid": "_:open", "dgraph.type": "Channel", "ch_uuid": open.String(), "ch_name": "open",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}}},
		{"uid": "_:crew", "dgraph.type": "Dm", "dm_grouping_id": crew,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:alex"}, {"uid": "_:kim"}}},
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

	// A person's conversation is grouped by the two ids, sorted and joined (GetGroupingId), not as a group is.
	pair := helpers.GetGroupingId(sam.String(), alex.String())
	count := func(q string, args ...any) int {
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	posts := func() int {
		return count(`SELECT count(*) FROM posts WHERE post_channel = $1 AND created_by = $2`, open, sam)
	}
	chats := func(grp string) int {
		return count(`SELECT count(*) FROM chats WHERE created_by = $1 AND grp_id = $2`, sam, grp)
	}
	channel := userAdapter.UserAndChannelFwdMessage{ChannelDgraphUid: uids["open"]}
	group := userAdapter.UserAndChannelFwdMessage{GrpId: crew}
	dm := userAdapter.UserAndChannelFwdMessage{UserUuid: alex.String()}
	for _, c := range []struct {
		name               string
		to                 []userAdapter.UserAndChannelFwdMessage
		post, crew, toAlex int
	}{
		{"a channel and a person", []userAdapter.UserAndChannelFwdMessage{channel, dm}, 1, 0, 1},
		{"a channel and a group", []userAdapter.UserAndChannelFwdMessage{channel, group}, 1, 1, 0},
		{"a channel, a group and a person", []userAdapter.UserAndChannelFwdMessage{channel, group, dm}, 1, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			p0, g0, a0 := posts(), chats(crew), chats(pair)
			raw, _ := json.Marshal(userAdapter.UserFwdMsgInput{
				HtmlText: "<p>ship it</p>", ChannelUuid: general.String(), PostUuid: source.String(), FwdTo: c.to,
			})
			rec := httptest.NewRecorder()
			userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asSam))
			if rec.Code != http.StatusOK {
				t.Fatalf("the forward answered %d: %s", rec.Code, rec.Body.String())
			}
			if p, g, a := posts()-p0, chats(crew)-g0, chats(pair)-a0; p != c.post || g != c.crew || a != c.toAlex {
				t.Fatalf("new rows: channel %d (want %d), group %d (want %d), person %d (want %d)", p, c.post, g, c.crew, a, c.toAlex)
			}
		})
	}
}
