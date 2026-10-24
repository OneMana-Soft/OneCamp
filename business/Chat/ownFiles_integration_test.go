//go:build integration

package business

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A reply in a DM's or a group's thread keeps only files its writer uploaded
// to that conversation (F4's rule, for chats): member B's reply naming member
// A's file, B's file of another place and an id nobody uploaded is written
// with B's own upload only, with its stored key.
func TestAConversationThreadReplyKeepsOnlyItsWritersOwnUploads(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupOpenSearch(t)
	dg := integration.SetupDgraph(t)

	a, b, c, private := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	dmRoot, groupRoot := uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{a, b, c} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, []string{"a", "b", "c"}[i]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	dm := helpers.GetGroupingId(a.String(), b.String())
	group, err := helpers.GenerateGroupID([]string{a.String(), b.String(), c.String()})
	if err != nil {
		t.Fatal(err)
	}
	for root, place := range map[uuid.UUID]string{dmRoot: dm, groupRoot: group} {
		if _, err := env.PG.Exec(`INSERT INTO chats (id, created_by, grp_id) VALUES ($1, $2, $3)`, root, a, place); err != nil {
			t.Fatal(err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:a", "dgraph.type": "User", "user_uuid": a.String(), "user_name": "ay", "user_deleted_at": live},
		{"uid": "_:b", "dgraph.type": "User", "user_uuid": b.String(), "user_name": "bee", "user_deleted_at": live},
		{"uid": "_:c", "dgraph.type": "User", "user_uuid": c.String(), "user_name": "cee", "user_deleted_at": live},
		{"uid": "_:d", "dgraph.type": "Dm", "dm_grouping_id": dm, "dm_participants": []map[string]any{{"uid": "_:a"}, {"uid": "_:b"}}},
		{"uid": "_:g", "dgraph.type": "Dm", "dm_grouping_id": group, "dm_participants": []map[string]any{{"uid": "_:a"}, {"uid": "_:b"}, {"uid": "_:c"}}},
		{"uid": "_:m", "dgraph.type": "Chat", "chat_uuid": dmRoot.String(), "chat_body_text": "<p>hi</p>", "chat_deleted_at": live,
			"chat_from": map[string]any{"uid": "_:a"}, "chat_to": map[string]any{"uid": "_:b"}, "chat_dm": map[string]any{"uid": "_:d"}},
		{"uid": "_:n", "dgraph.type": "Chat", "chat_uuid": groupRoot.String(), "chat_body_text": "<p>all</p>", "chat_deleted_at": live,
			"chat_from": map[string]any{"uid": "_:a"}, "chat_dm": map[string]any{"uid": "_:g"}},
	})
	file := func(by uuid.UUID, srcKey, srcValue, key string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`, id, key, srcKey, srcValue, by); err != nil {
			t.Fatal(err)
		}
		return id
	}
	inDM := file(b, "chat", dm, b.String()+"/reply.png")
	inGroup := file(b, "grpChat", group, b.String()+"/group.png")
	theirs := file(a, "channel", private.String(), a.String()+"/salaries.xlsx")
	elsewhere := file(b, "channel", private.String(), b.String()+"/elsewhere.png")
	att := func(id uuid.UUID, key string) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: id.String(), ObjectKey: key, FileName: key}
	}
	bee := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: b}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["b"], Uuid: b.String(), UserName: "bee"}}

	// reply is B's reply in root's thread of the conversation place naming
	// mine, and the files B's reply keeps as written.
	reply := func(root uuid.UUID, place string, mine uuid.UUID) []string {
		t.Helper()
		chat, err := GetDgraphChatBasicByUUID(ctx, root.String(), uids["b"])
		if err != nil || chat == nil {
			t.Fatalf("the message: %+v %v", chat, err)
		}
		created, err := CreateChatComment(ctx, &adapter.InputCreateOrUpdateCommentToChat{
			ChatUuid: root.String(), HTMLText: "<p>here</p>",
			MediaObj: []*dgraphStruct.DgraphAttachment{att(mine, "forged/key.png"), att(theirs, a.String()+"/salaries.xlsx"), att(elsewhere, b.String()+"/elsewhere.png"), att(uuid.New(), "made/up.png")},
		}, bee, nil, place, uids["b"], chat)
		if err != nil || created == nil {
			t.Fatalf("the reply: %+v %v", created, err)
		}
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
			`query q($id: string) { c(func: eq(comment_uuid, $id)) { comment_attachments { attachment_uuid attachment_obj_key } } }`,
			map[string]string{"$id": created.Uuid})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			C []struct {
				Files []struct {
					ID  string `json:"attachment_uuid"`
					Key string `json:"attachment_obj_key"`
				} `json:"comment_attachments"`
			} `json:"c"`
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.C) != 1 {
			t.Fatalf("read back: %v %s", err, resp.Json)
		}
		var got []string
		for _, f := range out.C[0].Files {
			got = append(got, f.ID+" "+f.Key)
		}
		return got
	}

	if got, want := reply(dmRoot, dm, inDM), []string{inDM.String() + " " + b.String() + "/reply.png"}; !slices.Equal(got, want) {
		t.Errorf("the DM's reply keeps %v; want only B's own upload to the DM: %v", got, want)
	}
	if got, want := reply(groupRoot, group, inGroup), []string{inGroup.String() + " " + b.String() + "/group.png"}; !slices.Equal(got, want) {
		t.Errorf("the group's reply keeps %v; want only B's own upload to the group: %v", got, want)
	}
}
