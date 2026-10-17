//go:build integration
// +build integration

package integration_test

// Chat A0's server answers, against a real Postgres, Dgraph and Redis.
//
//   - The sidebar counts a channel's unread mentions of you (SN-8): posts by
//     others, after your read mark there, not deleted. A channel @mention
//     looked like any chatter.
//   - The sidebar reads no channel's newest post, and a DM's newest message is
//     its newest live one: a deleted message moved its conversation up (F-33).
//   - A channel's latest page says whether older messages exist (F-2): it
//     always said none, so a long channel looked as if it began ten posts back.
//   - Lookups that find nothing answer 400, 404 or 403, never a panic or an
//     empty 200 (F-15, F-16).
//   - A thread, a channel's or a DM's, says when its message and its replies
//     were edited, so each shows "(edited)" (MC-25, SD-27).
//   - A reply to someone who has left the workspace is refused for that
//     reason, never with "try again".
//   - Editing a reply puts its mentions back to the people it names now: the
//     edit looked its mention node up by a predicate no node has (F-23).
//
// Run: go test -tags=integration ./tests/integration/ -run TestChatA0 -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestChatA0ServerAnswers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me, maya := uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{me: "me", maya: "maya"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	launch, general, left := uuid.New(), uuid.New(), uuid.New()
	long, short, secret, unknown := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, ch := range []uuid.UUID{launch, general, left} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, ch, ch.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	at := func(h int) string { return base.Add(time.Duration(h) * time.Hour).Format(time.RFC3339Nano) }
	// My read marks: #launch at 9:00, #general at 19:00, and one left behind
	// in a channel I'm no longer in.
	for ch, h := range map[uuid.UUID]int{launch: 0, general: 10, left: 0} {
		if _, err := env.PG.Exec(`INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES ($1, $2, $3)`, me, ch, at(h)); err != nil {
			t.Fatal(err)
		}
	}

	const live = "0001-01-01T00:00:00Z"
	nodes := []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me",
			"user_channels": []map[string]any{{"uid": "_:launch"}, {"uid": "_:general"}, {"uid": "_:long"}, {"uid": "_:short"}},
			"user_dms":      []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya", "user_dms": []map[string]any{{"uid": "_:dm"}}},
	}
	channel := func(key string, id uuid.UUID, private any, members ...string) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Channel", "ch_uuid": id.String(), "ch_name": key, "ch_deleted_at": live}
		if private != nil {
			n["ch_private"] = private
		}
		var ms []map[string]any
		for _, m := range members {
			ms = append(ms, map[string]any{"uid": "_:" + m})
		}
		n["ch_members"] = ms
		return n
	}
	posts := map[string][]map[string]any{}
	// post adds a post to a channel, by someone, at an hour; mentioning me
	// adds the mention node a send writes, with the post's time.
	post := func(key, ch, by string, h int, mentionsMe, deleted bool) {
		deletedAt := live
		if deleted {
			deletedAt = at(h + 1)
		}
		p := map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": "<p>" + key + "</p>",
			"post_by": map[string]any{"uid": "_:" + by}, "post_channel": map[string]any{"uid": "_:" + ch},
			"post_created_at": at(h), "post_deleted_at": deletedAt}
		if mentionsMe {
			p["post_mentions"] = map[string]any{"uid": "_:m-" + key, "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": "_:me"}},
				"mention_post": map[string]any{"uid": "_:" + key}, "mention_post_uuid": p["post_uuid"], "mention_created_at": at(h)}
		}
		posts[ch] = append(posts[ch], p)
	}
	post("l-before", "launch", "maya", -1, true, false) // before my mark
	post("l-one", "launch", "maya", 1, true, false)
	post("l-two", "launch", "maya", 2, true, false)
	post("l-mine", "launch", "me", 3, true, false)       // my own
	post("l-gone", "launch", "maya", 4, true, true)      // deleted
	post("l-chatter", "launch", "maya", 5, false, false) // no mention
	post("g-read", "general", "maya", 9, true, false)    // before #general's mark
	post("g-new", "general", "maya", 11, true, false)
	post("x-left", "left", "maya", 1, true, false) // a channel I left
	for i := 0; i < 11; i++ {
		post("long-"+strconv.Itoa(i), "long", "maya", i, false, false)
	}
	for i := 0; i < 10; i++ {
		post("short-"+strconv.Itoa(i), "short", "maya", i, false, false)
	}
	post("s-1", "secret", "maya", 1, false, false)
	for ch, key := range map[string]uuid.UUID{"launch": launch, "general": general, "left": left, "long": long, "short": short} {
		members := []string{"maya", "me"}
		if ch == "left" {
			members = []string{"maya"}
		}
		n := channel(ch, key, false, members...)
		n["ch_posts"] = posts[ch]
		nodes = append(nodes, n)
	}
	sec := channel("secret", secret, true, "maya")
	sec["ch_posts"] = posts["secret"]
	nodes = append(nodes, sec)
	// A channel whose privacy never loaded, which I'm not in.
	nodes = append(nodes, channel("unknown", unknown, nil, "maya"))
	// A DM with Maya: her newest message was deleted after an older live one.
	// She edited the live one, and her reply to it.
	liveChat := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": helpers.GetGroupingId(me.String(), maya.String()),
		"dm_participants": []map[string]any{{"uid": "_:me"}, {"uid": "_:maya"}},
		"dm_chats": []map[string]any{
			{"uid": "_:c-live", "dgraph.type": "Chat", "chat_uuid": liveChat, "chat_body_text": "hi", "chat_created_at": at(1), "chat_updated_at": at(2), "chat_deleted_at": live,
				"chat_from": map[string]any{"uid": "_:maya"}, "chat_to": map[string]any{"uid": "_:me"}, "chat_dm": map[string]any{"uid": "_:dm"},
				"chat_comments": []map[string]any{{"uid": "_:dm-reply", "dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "<p>ok</p>",
					"comment_by": map[string]any{"uid": "_:maya"}, "comment_created_at": at(1), "comment_updated_at": at(3)}}},
			{"uid": "_:c-gone", "dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "oops", "chat_created_at": at(2), "chat_deleted_at": at(3),
				"chat_from": map[string]any{"uid": "_:maya"}, "chat_to": map[string]any{"uid": "_:me"}, "chat_dm": map[string]any{"uid": "_:dm"}},
		}})
	// A DM with someone who has left the workspace: it is mine, so only
	// their leaving refuses a reply. Not in my sidebar's DMs.
	gone, toGone := uuid.NewString(), uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:gone", "dgraph.type": "User", "user_uuid": gone, "user_name": "gone", "user_deleted_at": at(0)},
		map[string]any{"uid": "_:dm-gone", "dgraph.type": "Dm", "dm_grouping_id": helpers.GetGroupingId(me.String(), gone),
			"dm_participants": []map[string]any{{"uid": "_:me"}, {"uid": "_:gone"}},
			"dm_chats": []map[string]any{{"uid": "_:c-to-gone", "dgraph.type": "Chat", "chat_uuid": toGone, "chat_body_text": "bye", "chat_created_at": at(1),
				"chat_deleted_at": live, "chat_from": map[string]any{"uid": "_:me"}, "chat_to": map[string]any{"uid": "_:gone"}, "chat_dm": map[string]any{"uid": "_:dm-gone"}}}})
	// A message that isn't in any conversation.
	stray := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:stray", "dgraph.type": "Chat", "chat_uuid": stray, "chat_body_text": "?", "chat_created_at": at(1),
		"chat_deleted_at": live, "chat_from": map[string]any{"uid": "_:maya"}})
	// Maya's reply in #launch, with my reaction on it; she edited it, and
	// the message it answers.
	reply := uuid.NewString()
	posts["launch"][1]["post_updated_at"] = at(2)
	posts["launch"][1]["post_comments"] = []map[string]any{{"uid": "_:reply"}}
	// It mentions me, through the mention node a reply's send writes.
	nodes = append(nodes, map[string]any{"uid": "_:reply", "dgraph.type": "Comment", "comment_uuid": reply, "comment_text": "<p>yes</p>",
		"comment_by": map[string]any{"uid": "_:maya"}, "comment_post": map[string]any{"uid": "_:l-one"}, "comment_created_at": at(1), "comment_updated_at": at(3),
		"comment_mentions": map[string]any{"uid": "_:m-reply", "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": "_:me"}},
			"mention_comment": map[string]any{"uid": "_:reply"}, "mention_comment_uuid": reply, "mention_created_at": at(1)},
		"comment_reactions": []map[string]any{{"uid": "_:reaction", "dgraph.type": "Reaction", "reaction_emoji_id": "eyes", "reaction_added_by": map[string]any{"uid": "_:me"}}}})
	uids := dg.Mutate(t, nodes)

	var who userModels.UserInfo
	who.UserPostgresInfo.Id = me
	who.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["me"], Uuid: me.String(), UserName: "me"}
	call := func(handler http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		reqCtx := context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rctx), helpers.UserInfoContextKey, who)
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(reqCtx))
		return rec.Code, rec.Body.String()
	}

	t.Run("the sidebar counts a channel's unread mentions of you", func(t *testing.T) {
		u, err := userBusiness.GetDgraphUserInfoByUUIDForSidebarNav(ctx, me)
		if err != nil || u == nil {
			t.Fatalf("the sidebar: %v", err)
		}
		got := map[string]uint64{}
		for _, c := range u.Channels {
			got[c.Uuid] = c.UnreadMentionCount
			if len(c.Posts) > 0 {
				t.Errorf("#%s carries its posts: the sidebar reads no channel's newest post", c.Name)
			}
		}
		if got[launch.String()] != 2 || got[general.String()] != 1 || got[long.String()] != 0 {
			t.Errorf("mention counts: #launch %d (want 2), #general %d (want 1), #long %d (want 0)", got[launch.String()], got[general.String()], got[long.String()])
		}
		if _, in := got[left.String()]; in {
			t.Error("a channel I left is in my sidebar")
		}
		if len(u.DMs) != 1 || len(u.DMs[0].Chats) != 1 || u.DMs[0].Chats[0].CreatedAt == nil ||
			u.DMs[0].Chats[0].CreatedAt.UTC().Format(time.RFC3339Nano) != at(1) {
			t.Errorf("the DM's newest message should be the live one at %s: %+v", at(1), u.DMs)
		}
	})

	t.Run("the latest page says whether there is more", func(t *testing.T) {
		for ch, want := range map[uuid.UUID]bool{long: true, short: false} {
			code, body := call(postController.GetLatestPosts, map[string]string{"channel_uuid": ch.String()}, nil)
			var answer pageAnswer
			if code != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil {
				t.Fatalf("latest page: %d %s", code, body)
			}
			if len(answer.Data.Posts) != 10 || answer.Data.HasMore != want {
				t.Errorf("a channel of %d posts: %d on its latest page, has_more %v; want 10 and %v", map[bool]int{true: 11, false: 10}[want], len(answer.Data.Posts), answer.Data.HasMore, want)
			}
		}
	})

	t.Run("lookups that find nothing answer, and never panic", func(t *testing.T) {
		stamp := strconv.FormatInt(base.Unix(), 10)
		cases := []struct {
			name    string
			handler http.HandlerFunc
			params  map[string]string
			body    any
			want    int
		}{
			{"older page of a channel id that isn't one", postController.GetOldPosts, map[string]string{"channel_uuid": "nope", "time_stamp": stamp}, nil, http.StatusBadRequest},
			{"newer page of a channel id that isn't one", postController.GetNewPosts, map[string]string{"channel_uuid": "nope", "time_stamp": stamp}, nil, http.StatusBadRequest},
			{"a linked message's page, channel id not one", postController.GetNewPostsWithCurrentPost, map[string]string{"channel_uuid": "nope", "post_uuid": uuid.NewString()}, nil, http.StatusBadRequest},
			{"a linked message's page, message id not one", postController.GetNewPostsWithCurrentPost, map[string]string{"channel_uuid": launch.String(), "post_uuid": "nope"}, nil, http.StatusBadRequest},
			{"a channel that isn't there", postController.GetLatestPosts, map[string]string{"channel_uuid": uuid.NewString()}, nil, http.StatusNotFound},
			{"a private channel I'm not in", postController.GetLatestPosts, map[string]string{"channel_uuid": secret.String()}, nil, http.StatusForbidden},
			{"a channel whose privacy never loaded", postController.GetOldPosts, map[string]string{"channel_uuid": unknown.String(), "time_stamp": stamp}, nil, http.StatusForbidden},
			{"a linked message in another channel", postController.GetNewPostsWithCurrentPost, map[string]string{"channel_uuid": general.String(), "post_uuid": posts["launch"][1]["post_uuid"].(string)}, nil, http.StatusNotFound},
			{"a linked message that was deleted", postController.GetNewPostsWithCurrentPost, map[string]string{"channel_uuid": launch.String(), "post_uuid": posts["launch"][4]["post_uuid"].(string)}, nil, http.StatusNotFound},
			{"a linked message", postController.GetNewPostsWithCurrentPost, map[string]string{"channel_uuid": launch.String(), "post_uuid": posts["launch"][1]["post_uuid"].(string)}, nil, http.StatusOK},
			{"adding someone to a channel that isn't there", channelController.AddChannelMember, nil, channelAdapter.InputChannelMemberInfo{ChannelUuid: uuid.NewString(), UserUuid: maya.String()}, http.StatusNotFound},
			{"replying to a message that isn't there", chatController.CreateChatComment, nil, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: uuid.NewString(), HTMLText: "<p>hi</p>"}, http.StatusNotFound},
			{"replying to a message in no conversation", chatController.CreateChatComment, nil, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: stray, HTMLText: "<p>hi</p>"}, http.StatusNotFound},
			{"the thread of a message in no conversation", chatController.GetDgraphChatByUUIDWithAllComments, map[string]string{"chat_uuid": stray}, nil, http.StatusNotFound},
			{"replying to someone who has left", chatController.CreateChatComment, nil, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: toGone, HTMLText: "<p>hi</p>"}, http.StatusConflict},
			// A channel id that isn't one was answered with nothing at all, an
			// empty 200; one that names nothing, as a request gone wrong (400).
			{"removing someone, channel id not one", channelController.RemoveChannelMember, nil, channelAdapter.InputChannelMemberInfo{ChannelUuid: "nope", UserUuid: maya.String()}, http.StatusBadRequest},
			{"removing someone from a channel that isn't there", channelController.RemoveChannelMember, nil, channelAdapter.InputChannelMemberInfo{ChannelUuid: uuid.NewString(), UserUuid: maya.String()}, http.StatusNotFound},
			{"an admin's role in a channel that isn't there", channelController.RemoveChannelModerator, nil, channelAdapter.InputChannelMemberInfo{ChannelUuid: uuid.NewString(), UserUuid: maya.String()}, http.StatusNotFound},
			{"who posts in a channel that isn't there", channelController.SetChannelPostPolicy, nil, map[string]string{"channel_uuid": uuid.NewString(), "post_policy": "everyone"}, http.StatusNotFound},
			{"restoring a channel that isn't there", channelController.RestoreChannel, nil, map[string]string{"channel_uuid": uuid.NewString()}, http.StatusNotFound},
		}
		for _, c := range cases {
			if code, body := call(c.handler, c.params, c.body); code != c.want {
				t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
			}
		}
	})

	// A channel that couldn't be read is a failure, for the app to try again:
	// it was answered as gone (404) or as a request gone wrong (400).
	t.Run("a channel that couldn't be read is a 503", func(t *testing.T) {
		gone, cancel := context.WithCancel(ctx)
		cancel()
		for name, c := range map[string]struct {
			handler http.HandlerFunc
			body    any
		}{
			"restoring it":     {channelController.RestoreChannel, map[string]string{"channel_uuid": launch.String()}},
			"who posts in it":  {channelController.SetChannelPostPolicy, map[string]string{"channel_uuid": launch.String(), "post_policy": "everyone"}},
			"removing someone": {channelController.RemoveChannelMember, channelAdapter.InputChannelMemberInfo{ChannelUuid: launch.String(), UserUuid: maya.String()}},
			"an admin's role":  {channelController.RemoveChannelModerator, channelAdapter.InputChannelMemberInfo{ChannelUuid: launch.String(), UserUuid: maya.String()}},
		} {
			raw, _ := json.Marshal(c.body)
			rec := httptest.NewRecorder()
			c.handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(gone, helpers.UserInfoContextKey, who)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s, the read failing: %d %s, want 503", name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("a thread says when its message and its replies were edited", func(t *testing.T) {
		code, body := call(postController.PostByUUIDWithAllComments, map[string]string{"post_uuid": posts["launch"][1]["post_uuid"].(string)}, nil)
		var answer struct {
			Data struct {
				UpdatedAt string `json:"post_updated_at"`
				Comments  []struct {
					UpdatedAt string `json:"comment_updated_at"`
				} `json:"post_comments"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil {
			t.Fatalf("the thread: %d %s", code, body)
		}
		if answer.Data.UpdatedAt != at(2) || len(answer.Data.Comments) != 1 || answer.Data.Comments[0].UpdatedAt != at(3) {
			t.Errorf("the thread's edit times: message %q, replies %+v; want %s and %s", answer.Data.UpdatedAt, answer.Data.Comments, at(2), at(3))
		}
	})

	t.Run("a DM thread says when its message and its replies were edited", func(t *testing.T) {
		code, body := call(chatController.GetDgraphChatByUUIDWithAllComments, map[string]string{"chat_uuid": liveChat}, nil)
		var answer struct {
			Data struct {
				UpdatedAt string `json:"chat_updated_at"`
				Comments  []struct {
					UpdatedAt string `json:"comment_updated_at"`
				} `json:"chat_comments"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil {
			t.Fatalf("the DM thread: %d %s", code, body)
		}
		if answer.Data.UpdatedAt != at(2) || len(answer.Data.Comments) != 1 || answer.Data.Comments[0].UpdatedAt != at(3) {
			t.Errorf("the DM thread's edit times: message %q, replies %+v; want %s and %s", answer.Data.UpdatedAt, answer.Data.Comments, at(2), at(3))
		}
	})

	t.Run("removing a reaction from a reply says so", func(t *testing.T) {
		code, body := call(postController.DeletePostCommentReaction, nil, postAdapter.InputDeleteReactionForPostComment{Uuid: reply, ReactionDgraphUid: uids["reaction"]})
		var answer map[string]any
		if code != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil || answer["msg"] == nil {
			t.Errorf("removing my reaction answered %d %q; want a 200 that says so", code, body)
		}
	})

	t.Run("editing a reply puts its mentions back to the people it names now", func(t *testing.T) {
		// The edit as a reply's edit writes it (business/Post UpdatePostComment):
		// it now names Maya, not me.
		now := base.Add(4 * time.Hour)
		edit := dgraphStruct.DgraphComment{Uid: "uid(co)", Uuid: reply, DType: []string{"Comment"}, Text: "<p>yes, @maya</p>", UpdatedAt: &now,
			Mentions: &dgraphStruct.DgraphMentions{Uid: "uid(me)", Comment: &dgraphStruct.DgraphComment{Uid: "uid(co)"}, CommentUuid: reply,
				Mentions: []*dgraphStruct.DgraphUser{{DType: []string{"User"}, Uid: uids["maya"]}}}}
		if err := commentDomain.UpdateDgraphCommentAndResetMentions(ctx, &edit, reply); err != nil {
			t.Fatal(err)
		}
		var answer struct {
			Mentions []struct {
				Users []struct {
					UUID string `json:"user_uuid"`
				} `json:"mention_users"`
			} `json:"m"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ m(func: eq(mention_comment_uuid, "`+reply+`")) { mention_users { user_uuid } } }`), &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer.Mentions) != 1 || len(answer.Mentions[0].Users) != 1 || answer.Mentions[0].Users[0].UUID != maya.String() {
			t.Errorf("the reply's mentions after the edit: %+v; want one mention of Maya, and none of me", answer.Mentions)
		}
	})
}
