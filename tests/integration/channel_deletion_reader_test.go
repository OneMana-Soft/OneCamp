//go:build integration

package integration_test

// A channel's deletion time is read one way everywhere (helpers.IsSoftDeleted):
// none at all, or the graph's zero time (the epoch), is a live channel, and any
// later time a deleted one. The post routes read it through a pointer, so a
// channel written without ch_deleted_at (an older import, a seed) panicked the
// reply, reaction and edit routes, and one written with the epoch was refused
// as deleted, by the routes and by the AI's send. Such a channel now takes
// its members' writes, and still nobody else's. A channel deleted for real
// still takes no reply, reaction or edit, from the app or from the AI's send.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	_ "github.com/akashc777/OneCamp/business/AI"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// deletionSeed writes a channel Jonas and Maya are in, its ch_deleted_at as
// given ("" for none), with one post of Jonas's, his reply under it, and a
// reply of Sam's from when he was in it. It answers the channel, the post and
// the two replies.
func deletionSeed(t *testing.T, w *b25World, deletedAt string) (channel, post, reply, samsReply string) {
	t.Helper()
	channel, post, reply, samsReply = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	name := "imported-" + channel[:8]
	now := time.Now().UTC().Format(time.RFC3339)
	comment := func(id, by, text string) map[string]any {
		return map[string]any{"dgraph.type": "Comment", "comment_uuid": id, "comment_text": text,
			"comment_by": map[string]any{"uid": w.uids[by]}, "comment_post": map[string]any{"uid": "_:p"},
			"comment_created_at": now, "comment_deleted_at": liveAt}
	}
	ch := map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": name,
		"ch_members": []map[string]any{{"uid": w.uids["maya"]}, {"uid": w.uids["jonas"]}},
		"ch_posts": []map[string]any{{"uid": "_:p", "dgraph.type": "Post", "post_uuid": post, "post_text": "<p>The plan</p>",
			"post_by": map[string]any{"uid": w.uids["jonas"]}, "post_channel": map[string]any{"uid": "_:ch"},
			"post_created_at": now, "post_deleted_at": liveAt,
			"post_comments": []map[string]any{comment(reply, "jonas", "<p>Friday?</p>"), comment(samsReply, "sam", "<p>Count me in</p>")}}}}
	if deletedAt != "" {
		ch["ch_deleted_at"] = deletedAt
	}
	w.dg.Mutate(t, ch)
	// The AI's send writes its post to Postgres, where posts.post_channel needs
	// the channel's row (names are unique there). Whether the channel is
	// deleted stays the graph's to say: the routes and the send read it there.
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, false, $3)`, channel, name, w.people["jonas"]); err != nil {
		t.Fatal(err)
	}
	return channel, post, reply, samsReply
}

// sendAs runs the AI's send_message for name in channel, and answers what it
// says it posted.
func sendAs(t *testing.T, w *b25World, name, channel string) (map[string]string, error) {
	t.Helper()
	send, ok := ai.GetExecutor("send_message")
	if !ok {
		t.Fatal("no send_message tool")
	}
	_, sent, err := send(context.Background(), ai.ProposedAction{ToolName: "send_message",
		Params: map[string]string{"channel_uuid": channel, "text": "Friday works"}}, w.people[name].String())
	return sent, err
}

// postChannel answers the channel a post is in, from the graph.
func postChannel(t *testing.T, w *b25World, post string) string {
	t.Helper()
	var got struct {
		P []struct {
			C struct {
				UUID string `json:"ch_uuid"`
			} `json:"post_channel"`
		}
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { post_channel { ch_uuid } } }`, post)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.P) != 1 {
		return ""
	}
	return got.P[0].C.UUID
}

func TestAChannelWithNoDeletionTimeTakesReplies(t *testing.T) {
	w := newB25World(t)
	for _, c := range []struct{ name, deletedAt string }{
		{"no deletion time", ""},
		{"the graph's zero time", "1970-01-01T00:00:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			channel, post, _, samsReply := deletionSeed(t, w, c.deletedAt)
			code, data, msg := w.call(t, "jonas", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: "<p>Friday works</p>"})
			var reply struct {
				Uuid string `json:"comment_id"`
			}
			if err := json.Unmarshal(data, &reply); code != http.StatusOK || err != nil || reply.Uuid == "" {
				t.Fatalf("a reply in a live channel: %d %s %v", code, msg, err)
			}
			if code, _, msg := w.call(t, "maya", postController.CreateOrUpdateReactionOnCommentPost, http.MethodPost, postAdapter.InputUpdateReactionForCommentInPost{Uuid: reply.Uuid, EmojiUuid: "+1"}); code != http.StatusOK {
				t.Errorf("a reaction to that reply: %d %s", code, msg)
			}
			if code, _, msg := w.call(t, "jonas", postController.UpdateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply.Uuid, HTMLText: "<p>Friday works for me</p>"}); code != http.StatusOK {
				t.Errorf("an edit of that reply: %d %s", code, msg)
			}
			// The AI's send for a member posts there too: it read the epoch as deleted.
			sent, err := sendAs(t, w, "jonas", channel)
			if err != nil || sent["post_uuid"] == "" {
				t.Fatalf("the AI's send in a live channel: %v", err)
			}
			if got := postChannel(t, w, sent["post_uuid"]); got != channel {
				t.Errorf("the AI's post is in channel %q, not %q", got, channel)
			}

			// Sam isn't in it any more: his reply, reaction, edit of his own old
			// reply and the AI's send for him are all refused.
			if code, _, _ := w.call(t, "sam", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: "<p>From outside</p>"}); code == http.StatusOK {
				t.Error("a non-member's reply was taken")
			}
			if code, _, _ := w.call(t, "sam", postController.CreateOrUpdateReactionOnCommentPost, http.MethodPost, postAdapter.InputUpdateReactionForCommentInPost{Uuid: reply.Uuid, EmojiUuid: "+1"}); code == http.StatusOK {
				t.Error("a non-member's reaction was taken")
			}
			if got := w.reactionsOn(t, "comment_uuid", "comment_reactions", reply.Uuid); !reflect.DeepEqual(got, []string{"maya:+1"}) {
				t.Errorf("the reply's reactions after a non-member's: %v", got)
			}
			if code, _, _ := w.call(t, "sam", postController.UpdateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: samsReply, HTMLText: "<p>Changed from outside</p>"}); code == http.StatusOK {
				t.Error("a non-member's edit of his own old reply was taken")
			}
			if _, err := sendAs(t, w, "sam", channel); err == nil {
				t.Error("the AI's send for a non-member posted")
			}
		})
	}
}

// A channel deleted for real takes nothing, from the routes or from the AI's
// send, whoever in it asks, and the reply already there takes no reaction.
func TestADeletedChannelTakesNoReplyReactionEditOrSend(t *testing.T) {
	w := newB25World(t)
	channel, post, reply, _ := deletionSeed(t, w, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if code, _, _ := w.call(t, "jonas", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: "<p>Friday works</p>"}); code == http.StatusOK {
		t.Error("a reply in a deleted channel was taken")
	}
	if code, _, _ := w.call(t, "maya", postController.CreateOrUpdateReactionOnCommentPost, http.MethodPost, postAdapter.InputUpdateReactionForCommentInPost{Uuid: reply, EmojiUuid: "+1"}); code == http.StatusOK {
		t.Error("a reaction to a reply in a deleted channel was taken")
	}
	if code, _, _ := w.call(t, "jonas", postController.UpdateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply, HTMLText: "<p>Changed</p>"}); code == http.StatusOK {
		t.Error("an edit of a reply in a deleted channel was taken")
	}
	if got := w.reactionsOn(t, "comment_uuid", "comment_reactions", reply); len(got) != 0 {
		t.Errorf("the reply in a deleted channel has reactions: %v", got)
	}
	if _, err := sendAs(t, w, "jonas", channel); err == nil {
		t.Error("the AI's send posted in a deleted channel")
	}
}
