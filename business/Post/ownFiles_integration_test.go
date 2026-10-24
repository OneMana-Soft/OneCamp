//go:build integration

package business

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A reply in a channel's thread keeps only files its writer uploaded to the
// channel (F4's rule, for posts): member B's reply naming member A's file,
// B's file of another place and an id nobody uploaded wrote entries that
// opened for no one, or for the wrong people. B's own upload stays, with its
// stored key.
func TestAThreadReplyKeepsOnlyItsWritersOwnUploads(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupOpenSearch(t)
	dg := integration.SetupDgraph(t)

	a, b, launch, private, post := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{a, b} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, []string{"a", "b"}[i]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:a", "dgraph.type": "User", "user_uuid": a.String(), "user_name": "ay", "user_deleted_at": live},
		{"uid": "_:b", "dgraph.type": "User", "user_uuid": b.String(), "user_name": "bee", "user_deleted_at": live},
		{"uid": "_:l", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch", "ch_private": false,
			"ch_post_policy": "everyone", "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:a"}, {"uid": "_:b"}}},
		{"uid": "_:p", "dgraph.type": "Post", "post_uuid": post.String(), "post_text": "<p>the plan</p>", "post_deleted_at": live,
			"post_by": map[string]any{"uid": "_:a"}, "post_channel": map[string]any{"uid": "_:l"}},
	})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, []any{launch}},
		{`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, []any{post, launch, a}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	file := func(by uuid.UUID, srcKey, srcValue, key string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`, id, key, srcKey, srcValue, by); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mine := file(b, "channel", launch.String(), b.String()+"/reply.png")
	theirs := file(a, "channel", private.String(), a.String()+"/salaries.xlsx")
	elsewhere := file(b, "channel", private.String(), b.String()+"/elsewhere.png")
	att := func(id uuid.UUID, key string) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: id.String(), ObjectKey: key, FileName: key}
	}

	dgraphPost, err := GetDgraphPostByUUID(ctx, post.String(), uids["b"])
	if err != nil || dgraphPost == nil || dgraphPost.Channel == nil {
		t.Fatalf("the post: %+v %v", dgraphPost, err)
	}
	bee := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: b}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["b"], Uuid: b.String(), UserName: "bee"}}
	created, err := CreatePostComment(ctx, &adapter.InputCreateOrUpdateCommentToPost{
		PostUuid: post.String(), HTMLText: "<p>here</p>",
		MediaObj: []*dgraphStruct.DgraphAttachment{att(mine, "forged/key.png"), att(theirs, a.String()+"/salaries.xlsx"), att(elsewhere, b.String()+"/elsewhere.png"), att(uuid.New(), "made/up.png")},
	}, bee, nil, dgraphPost)
	if err != nil || created == nil {
		t.Fatalf("the reply: %+v %v", created, err)
	}
	reply := created.Uuid

	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
		`query q($id: string) { c(func: eq(comment_uuid, $id)) { comment_attachments { attachment_uuid attachment_obj_key } } }`,
		map[string]string{"$id": reply})
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
	if want := []string{mine.String() + " " + b.String() + "/reply.png"}; !slices.Equal(got, want) {
		t.Errorf("the reply keeps %v; want only B's own upload to #launch: %v", got, want)
	}
}
