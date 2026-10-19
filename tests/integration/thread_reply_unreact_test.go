//go:build integration
// +build integration

package integration_test

// Reacting to a thread reply and taking the reaction back, as the thread
// panel does it, in a channel, a DM and a group: react, change the emoji,
// take it back; each step answers 200 and the graph holds what was asked.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAThreadReplysReactionCanBeTakenBack -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAThreadReplysReactionCanBeTakenBack(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	// Ana and Ben in #general, where Ben's post has a reply of Ben's; their
	// DM, where Ben's message has a reply of Ben's; and a group of three,
	// the same.
	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New(), "cy": uuid.New()}
	channel, post, postReply := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dmChat, dmReply, groupChat, groupReply := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	dmID := helpers.GetGroupingId(people["ana"].String(), people["ben"].String())
	groupID := "grp-" + uuid.NewString()[:8]
	live, at := "0001-01-01T00:00:00Z", "2026-10-01T10:00:00Z"
	reply := func(blank, id string, on map[string]any) map[string]any {
		r := map[string]any{"uid": blank, "dgraph.type": "Comment", "comment_uuid": id, "comment_text": "<p>agreed</p>",
			"comment_by": map[string]any{"uid": "_:ben"}, "comment_created_at": at, "comment_deleted_at": live}
		for k, v := range on {
			r[k] = v
		}
		return r
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": people["ana"].String(), "user_name": "ana", "user_deleted_at": live},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": people["ben"].String(), "user_name": "ben", "user_deleted_at": live},
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": people["cy"].String(), "user_name": "cy", "user_deleted_at": live},
		{"uid": "_:channel", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "general", "ch_private": false,
			"ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}},
			"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": post, "post_text": "<p>friday</p>",
				"post_by": map[string]any{"uid": "_:ben"}, "post_channel": map[string]any{"uid": "_:channel"},
				"post_created_at": at, "post_deleted_at": live,
				"post_comments": []map[string]any{reply("_:postReply", postReply, map[string]any{"comment_post": map[string]any{"uid": "_:post"}})}}}},
		{"uid": "_:dm", "dgraph.type": "DM", "dm_grouping_id": dmID, "dm_participants": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}},
			"dm_chats": []map[string]any{{"uid": "_:dmChat", "dgraph.type": "Chat", "chat_uuid": dmChat, "chat_text": "<p>lunch?</p>",
				"chat_from": map[string]any{"uid": "_:ben"}, "chat_to": map[string]any{"uid": "_:ana"}, "chat_dm": map[string]any{"uid": "_:dm"},
				"chat_created_at": at, "chat_deleted_at": live,
				"chat_comments": []map[string]any{reply("_:dmReply", dmReply, map[string]any{"comment_chat": map[string]any{"uid": "_:dmChat"},
					"comment_chat_grouping_id": dmID})}}}},
		{"uid": "_:group", "dgraph.type": "DM", "dm_grouping_id": groupID,
			"dm_participants": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}, {"uid": "_:cy"}},
			"dm_chats": []map[string]any{{"uid": "_:groupChat", "dgraph.type": "Chat", "chat_uuid": groupChat, "chat_text": "<p>offsite?</p>",
				"chat_from": map[string]any{"uid": "_:ben"}, "chat_dm": map[string]any{"uid": "_:group"},
				"chat_created_at": at, "chat_deleted_at": live,
				"chat_comments": []map[string]any{reply("_:groupReply", groupReply, map[string]any{"comment_chat": map[string]any{"uid": "_:groupChat"},
					"comment_chat_grouping_id": groupID})}}}},
	})

	call := func(who string, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u)))
		return rec.Code, rec.Body.String()
	}
	// held is the reactions on the reply, "emoji by uid".
	held := func(reply string) []string {
		t.Helper()
		var got struct {
			P []struct {
				R []struct {
					Uid   string `json:"uid"`
					Emoji string `json:"reaction_emoji_id"`
				} `json:"r"`
			}
		}
		q := fmt.Sprintf(`{ p(func: eq(comment_uuid, %q)) { r: comment_reactions { uid reaction_emoji_id } } }`, reply)
		if err := json.Unmarshal(dg.Query(t, q), &got); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range got.P {
			for _, r := range p.R {
				out = append(out, r.Emoji+" "+r.Uid)
			}
		}
		return out
	}

	for _, c := range []struct {
		name           string
		reply          string
		react, unreact http.HandlerFunc
		reactBody      func(emoji, uid string) any
		unreactBody    func(uid string) any
	}{
		{"a channel's thread", postReply, postController.CreateOrUpdateReactionOnCommentPost, postController.DeletePostCommentReaction,
			func(emoji, uid string) any {
				return postAdapter.InputUpdateReactionForCommentInPost{Uuid: postReply, EmojiUuid: emoji, ReactionDgraphUid: uid}
			},
			func(uid string) any {
				return postAdapter.InputDeleteReactionForPostComment{Uuid: postReply, ReactionDgraphUid: uid}
			}},
		{"a DM's thread", dmReply, chatController.CreateOrUpdateChatCommentReaction, chatController.DeleteReactionOnCommentChat,
			func(emoji, uid string) any {
				return chatAdapter.InputUpdateReactionForCommentInChat{Uuid: dmReply, EmojiReactionUuid: emoji, ReactionDgraphUid: uid}
			},
			func(uid string) any {
				return chatAdapter.InputUpdateReactionForCommentInChat{Uuid: dmReply, ReactionDgraphUid: uid}
			}},
		{"a group's thread", groupReply, chatController.CreateOrUpdateChatCommentReaction, chatController.DeleteReactionOnCommentChat,
			func(emoji, uid string) any {
				return chatAdapter.InputUpdateReactionForCommentInChat{Uuid: groupReply, EmojiReactionUuid: emoji, ReactionDgraphUid: uid}
			},
			func(uid string) any {
				return chatAdapter.InputUpdateReactionForCommentInChat{Uuid: groupReply, ReactionDgraphUid: uid}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out := call("ana", c.react, c.reactBody("thumbs-up", ""))
			var answer struct {
				Data struct {
					Uid string `json:"reaction_dgraph_id"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(out), &answer); code != http.StatusOK || err != nil || answer.Data.Uid == "" {
				t.Fatalf("Ana reacting: %d %s", code, out)
			}
			mine := answer.Data.Uid
			if got := held(c.reply); len(got) != 1 || got[0] != "thumbs-up "+mine {
				t.Fatalf("after reacting the reply holds %v, want her thumbs-up %s", got, mine)
			}
			// She changes the emoji: the same reaction, and the answer names it.
			code, out = call("ana", c.react, c.reactBody("party", mine))
			answer.Data.Uid = ""
			if err := json.Unmarshal([]byte(out), &answer); code != http.StatusOK || err != nil || answer.Data.Uid != mine {
				t.Errorf("changing her reaction: %d %s, want it to name %s", code, out, mine)
			}
			// And takes it back.
			if code, out := call("ana", c.unreact, c.unreactBody(mine)); code != http.StatusOK {
				t.Fatalf("taking her reaction back: %d %s", code, out)
			}
			if got := held(c.reply); len(got) != 0 {
				t.Errorf("after taking it back the reply holds %v", got)
			}
		})
	}
}
