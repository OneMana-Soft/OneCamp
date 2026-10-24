//go:build integration

package business

import (
	"context"
	"slices"
	"testing"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A message keeps only files its writer uploaded to its conversation (F4's
// rule, for posts and chats). Member B's channel post, a thread reply's copy
// in the channel ("Also send to #channel"), DM and group message each name
// B's own upload to that conversation, member A's file (uploaded to
// A's private channel), a file B uploaded somewhere else and an id nobody
// uploaded. A file is served through its place, so the others opened for no
// one, or for the wrong people: only B's own upload is kept, with the storage
// key its upload stored, whatever key the request named.
func TestAMessageKeepsOnlyItsWritersOwnUploads(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)

	a, b, c := uuid.New(), uuid.New(), uuid.New()
	launch, private := uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{a, b, c} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, []string{"a", "b", "c"}[i]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:a", "dgraph.type": "User", "user_uuid": a.String(), "user_name": "ay", "user_deleted_at": live},
		{"uid": "_:b", "dgraph.type": "User", "user_uuid": b.String(), "user_name": "bee", "user_deleted_at": live},
		{"uid": "_:c", "dgraph.type": "User", "user_uuid": c.String(), "user_name": "cee", "user_deleted_at": live},
		{"uid": "_:l", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch", "ch_private": false,
			"ch_post_policy": "everyone", "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:a"}, {"uid": "_:b"}}},
	})
	dm := helpers.GetGroupingId(a.String(), b.String())
	group, err := helpers.GenerateGroupID([]string{a.String(), b.String(), c.String()})
	if err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{"dgraph.type": "Dm", "dm_grouping_id": group,
		"dm_participants": []map[string]any{{"uid": uids["a"]}, {"uid": uids["b"]}, {"uid": uids["c"]}}}})

	file := func(by uuid.UUID, srcKey, srcValue, key string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`, id, key, srcKey, srcValue, by); err != nil {
			t.Fatal(err)
		}
		return id
	}
	theirs := file(a, "channel", private.String(), a.String()+"/salaries.xlsx")
	elsewhere := file(b, "channel", private.String(), b.String()+"/elsewhere.png")
	never := uuid.New()
	att := func(id uuid.UUID, key string) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: id.String(), ObjectKey: key, FileName: key}
	}
	files := func(mine uuid.UUID) []*dgraphStruct.DgraphAttachment {
		return []*dgraphStruct.DgraphAttachment{
			att(mine, "someone-else/forged-key.png"),
			att(theirs, a.String()+"/salaries.xlsx"),
			att(elsewhere, b.String()+"/elsewhere.png"),
			att(never, "made/up.png"),
		}
	}
	kept := func(list []*dgraphStruct.DgraphAttachment) []string {
		var out []string
		for _, f := range list {
			out = append(out, f.Uuid+" "+f.ObjectKey)
		}
		return out
	}
	bee := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: b}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["b"], Uuid: b.String()}}

	// A channel post.
	inChannel := file(b, "channel", launch.String(), b.String()+"/plan.png")
	post := &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: launch.String(), HTMLText: "<p>the plan</p>", MediaObj: files(inChannel)}
	if _, err := PrepareChannelPost(ctx, bee, post); err != nil {
		t.Fatal(err)
	}
	if got, want := kept(post.MediaObj), []string{inChannel.String() + " " + b.String() + "/plan.png"}; !slices.Equal(got, want) {
		t.Errorf("the post keeps %v; want only B's own upload to #launch: %v", got, want)
	}

	// A thread reply's copy in the channel.
	copied := &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: launch.String(), HTMLText: "<p>also here</p>", MediaObj: files(inChannel)}
	if _, err := PrepareReplyCopy(ctx, bee, copied); err != nil {
		t.Fatal(err)
	}
	if got, want := kept(copied.MediaObj), []string{inChannel.String() + " " + b.String() + "/plan.png"}; !slices.Equal(got, want) {
		t.Errorf("the reply's copy keeps %v; want only B's own upload to #launch: %v", got, want)
	}

	// A DM.
	inDM := file(b, "chat", dm, b.String()+"/dm.png")
	chat := &chatAdapter.ChatInfo{ToUuid: a.String(), TextHtml: "<p>hi</p>", MediaObjects: files(inDM)}
	if _, err := PrepareDirectMessage(ctx, bee, chat); err != nil {
		t.Fatal(err)
	}
	if got, want := kept(chat.MediaObjects), []string{inDM.String() + " " + b.String() + "/dm.png"}; !slices.Equal(got, want) {
		t.Errorf("the DM keeps %v; want only B's own upload to it: %v", got, want)
	}

	// A group message.
	inGroup := file(b, "grpChat", group, b.String()+"/group.png")
	grp := &chatAdapter.ChatInfo{GrpUuid: group, TextHtml: "<p>all</p>", MediaObjects: files(inGroup)}
	if _, err := PrepareGroupMessage(ctx, bee, grp); err != nil {
		t.Fatal(err)
	}
	if got, want := kept(grp.MediaObjects), []string{inGroup.String() + " " + b.String() + "/group.png"}; !slices.Equal(got, want) {
		t.Errorf("the group message keeps %v; want only B's own upload to it: %v", got, want)
	}
}
