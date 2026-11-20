//go:build integration

package integration_test

// A thread reply tried again is the same reply (V-1088). The app gives a reply
// an id when it shows it (client_msg_id); when the answer to it is lost and the
// app sends it again, the server, which has the first, answers it again and
// writes nothing: one reply, and one copy in the channel when it was also sent
// there. Another id is another reply, and so is the same id from someone else.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
)

// A reply sent again while its first try is still going is told so (409),
// as a post's or a chat's send is, never as a failed send (CR-373 F1).
func TestAThreadReplyWhoseFirstTryIsStillGoingSaysSo(t *testing.T) {
	w := newB25World(t)
	const id = "tab-1-going-1"
	ctx := context.Background()
	// going claims the send-once key the reply's first try holds while it runs.
	going := func(root string) {
		key := registry.SendOnce.Build(w.people["maya"].String(), "th:"+root, id)
		if err := redisInit.RedisClient.SetNX(ctx, key, "going", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { redisInit.RedisClient.Del(ctx, key) })
	}

	channel, post := uuid.NewString(), uuid.NewString()
	w.channel(t, channel, "going-"+channel[:8], false, "", []string{"maya", "jonas"}, nil, map[string]string{post: "jonas"})
	going(post)
	body := map[string]any{"post_id": post, "comment_text_html": "<p>Friday works</p>", "client_msg_id": id}
	if code, _, msg := w.call(t, "maya", postController.CreateCommentInPost, http.MethodPost, body); code != http.StatusConflict {
		t.Errorf("a post's thread reply whose first try is still going: HTTP %d %s, want 409", code, msg)
	}
	if got := countIn(t, w, "post_uuid", post, "post_comments"); got != 0 {
		t.Errorf("%d post replies written while the first try is going, want 0", got)
	}

	_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
	var chat struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(data, &chat) != nil || chat.UUID == "" {
		t.Fatalf("the DM: %s", data)
	}
	going(chat.UUID)
	body = map[string]any{"chat_id": chat.UUID, "comment_text_html": "<p>Noon</p>", "client_msg_id": id}
	if code, _, msg := w.call(t, "maya", chatController.CreateChatComment, http.MethodPost, body); code != http.StatusConflict {
		t.Errorf("a chat's thread reply whose first try is still going: HTTP %d %s, want 409", code, msg)
	}
	if got := countIn(t, w, "chat_uuid", chat.UUID, "chat_comments"); got != 0 {
		t.Errorf("%d chat replies written while the first try is going, want 0", got)
	}
}

// countIn answers count(edge) on the node whose pred is id, from the graph.
func countIn(t *testing.T, w *b25World, pred, id, edge string) int {
	t.Helper()
	var got struct {
		Q []struct {
			N int `json:"n"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(%s, %q)) { n: count(%s) } }`, pred, id, edge)), &got); err != nil || len(got.Q) != 1 {
		t.Fatalf("count(%s) of %s: %v %+v", edge, id, err, got)
	}
	return got.Q[0].N
}

// replyAs sends name's reply through handler with body, and answers the
// reply's id.
func replyAs(t *testing.T, w *b25World, name string, handler http.HandlerFunc, body map[string]any) string {
	t.Helper()
	code, data, msg := w.call(t, name, handler, http.MethodPost, body)
	var out struct {
		Uuid string `json:"comment_id"`
	}
	if err := json.Unmarshal(data, &out); code != http.StatusOK || err != nil || out.Uuid == "" {
		t.Fatalf("%s's reply: %d %s %v", name, code, msg, err)
	}
	return out.Uuid
}

func TestAThreadReplySentTwiceIsWrittenOnce(t *testing.T) {
	w := newB25World(t)
	channel, post := uuid.NewString(), uuid.NewString()
	name := "once-" + channel[:8]
	w.channel(t, channel, name, false, "", []string{"maya", "jonas"}, nil, map[string]string{post: "jonas"})
	// The copy in the channel is a post, whose row needs the channel's.
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, false, $3)`, channel, name, w.people["jonas"]); err != nil {
		t.Fatal(err)
	}
	reply := func(who, id string) string {
		body := map[string]any{"post_id": post, "comment_text_html": "<p>Friday works</p>", "also_send_to_channel": true}
		if id != "" {
			body["client_msg_id"] = id
		}
		return replyAs(t, w, who, postController.CreateCommentInPost, body)
	}
	check := func(when string, replies, posts int) {
		t.Helper()
		if got := countIn(t, w, "post_uuid", post, "post_comments"); got != replies {
			t.Errorf("%s: %d replies, want %d", when, got, replies)
		}
		if got := countIn(t, w, "ch_uuid", channel, "ch_posts"); got != posts {
			t.Errorf("%s: %d posts in the channel, want %d (the root and a copy per reply)", when, got, posts)
		}
	}

	first := reply("maya", "tab-1-reply-1")
	if again := reply("maya", "tab-1-reply-1"); again != first {
		t.Errorf("the reply sent again answers %q, not the first's %q", again, first)
	}
	check("one reply sent twice", 1, 2)
	if other := reply("maya", "tab-1-reply-2"); other == first {
		t.Error("another id answered the first reply")
	}
	check("another id", 2, 3)
	if jonas := reply("jonas", "tab-1-reply-1"); jonas == first {
		t.Error("someone else's same id answered Maya's reply")
	}
	check("the same id from someone else", 3, 4)
	reply("maya", "")
	reply("maya", "")
	check("two replies with no id", 5, 6)
}

func TestAChatThreadReplySentTwiceIsWrittenOnce(t *testing.T) {
	w := newB25World(t)
	_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
	var created struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(data, &created) != nil || created.UUID == "" {
		t.Fatalf("the DM: %s", data)
	}
	reply := func(who, id string) string {
		body := map[string]any{"chat_id": created.UUID, "comment_text_html": "<p>Noon</p>"}
		if id != "" {
			body["client_msg_id"] = id
		}
		return replyAs(t, w, who, chatController.CreateChatComment, body)
	}
	check := func(when string, replies int) {
		t.Helper()
		if got := countIn(t, w, "chat_uuid", created.UUID, "chat_comments"); got != replies {
			t.Errorf("%s: %d replies, want %d", when, got, replies)
		}
	}

	first := reply("maya", "tab-1-reply-1")
	if again := reply("maya", "tab-1-reply-1"); again != first {
		t.Errorf("the reply sent again answers %q, not the first's %q", again, first)
	}
	check("one reply sent twice", 1)
	if other := reply("maya", "tab-1-reply-2"); other == first {
		t.Error("another id answered the first reply")
	}
	check("another id", 2)
	if jonas := reply("jonas", "tab-1-reply-1"); jonas == first {
		t.Error("someone else's same id answered Maya's reply")
	}
	check("the same id from someone else", 3)
}
