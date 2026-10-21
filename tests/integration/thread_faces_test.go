//go:build integration
// +build integration

package integration_test

// A thread's faces come with its page (chat P2; found by DESIGN-BUILD-2, 18
// Oct). The page queries read each message's replies only for the newest
// one's time, so a thread's reply bar had faces only for replies that arrived
// during the session. Now a channel's and a DM's page carry up to three of a
// thread's people, each once, the last of them its latest live reply; a
// guest's reply is the guest's face, carrying the label naming them and never
// the reply; no email leaves the server.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAThreadsFacesComeWithItsPage -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
)

// pageReply is one of a page's replies as the web reads it: when, its
// author, and the label it carries.
type pageReply struct {
	At   time.Time `json:"comment_created_at"`
	Text string    `json:"comment_text"`
	By   struct {
		UUID  string `json:"user_uuid"`
		Name  string `json:"user_name"`
		Email string `json:"user_email_id"`
	} `json:"comment_by"`
}

func TestAThreadsFacesComeWithItsPage(t *testing.T) {
	w := newB25World(t)
	guests := w.dg.Mutate(t, map[string]any{"uid": "_:g", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "Guests",
		"is_bot": true, "user_email_id": userDomain.ChannelGuestBotEmail, "user_deleted_at": liveAt})["g"]

	t.Run("a channel's page", func(t *testing.T) {
		launch, plan := uuid.NewString(), uuid.NewString()
		w.channel(t, launch, "launch", false, "", []string{"maya", "jonas", "sam"}, nil, map[string]string{plan: "maya"})
		at := func(m int) string { return time.Date(2026, 10, 18, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
		reply := func(by, text string, m int, deleted bool) map[string]any {
			gone := liveAt
			if deleted {
				gone = at(m + 1)
			}
			return map[string]any{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": text,
				"comment_by": map[string]any{"uid": by}, "comment_created_at": at(m), "comment_deleted_at": gone}
		}
		// Oldest first: Sam, Maya, Jonas, Maya again, a guest through the Guests
		// bot, and a reply Sam deleted, the newest.
		var post struct {
			P []struct {
				UID string `json:"uid"`
			} `json:"p"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { uid } }`, plan)), &post); err != nil || len(post.P) != 1 {
			t.Fatalf("the post: %v", err)
		}
		w.dg.Mutate(t, map[string]any{"uid": post.P[0].UID, "post_comments": []map[string]any{
			reply(w.uids["sam"], "<p>First</p>", 1, false),
			reply(w.uids["maya"], "<p>Second</p>", 2, false),
			reply(w.uids["jonas"], "<p>Third</p>", 3, false),
			reply(w.uids["maya"], "<p>Fourth</p>", 4, false),
			reply(guests, "<p><strong>[Priya (Acme) (guest)]</strong></p><p>Looks good to us</p>", 5, false),
			reply(w.uids["sam"], "<p>Never mind</p>", 6, true),
		}})

		code, data, msg := w.call(t, "maya", postController.GetLatestPosts, http.MethodGet, nil, "channel_uuid", launch)
		if code != http.StatusOK {
			t.Fatalf("the page: %d %s", code, msg)
		}
		if strings.Contains(string(data), "@acme.test") || strings.Contains(string(data), userDomain.ChannelGuestBotEmail) {
			t.Fatalf("an email left the server: %s", data)
		}
		type pagePost struct {
			UUID     string      `json:"post_uuid"`
			Count    int         `json:"post_comment_count"`
			Comments []pageReply `json:"post_comments"`
		}
		var page struct {
			Posts []pagePost `json:"posts"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(page.Posts, func(p pagePost) bool { return p.UUID == plan })
		if i < 0 {
			t.Fatalf("the post isn't on its page: %s", data)
		}
		got := page.Posts[i]
		var who []string
		for _, c := range got.Comments {
			who = append(who, c.By.Name+c.Text)
		}
		want := []string{"jonas", "maya", "Guests<p><strong>[Priya (Acme) (guest)]</strong></p>"}
		if !slices.Equal(who, want) {
			t.Fatalf("faces %q, want %q: three people, Maya once, the guest as herself, never the deleted reply", who, want)
		}
		if last := got.Comments[len(got.Comments)-1]; !last.At.Equal(time.Date(2026, 10, 18, 10, 5, 0, 0, time.UTC)) {
			t.Errorf("the last face's reply was at %v: want the latest live reply's, 10:05", last.At)
		}
		if got.Count != 5 {
			t.Errorf("%d replies, want the 5 live ones", got.Count)
		}
	})

	t.Run("a DM's page", func(t *testing.T) {
		_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
		var created struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(data, &created) != nil || created.UUID == "" {
			t.Fatalf("the DM: %s", data)
		}
		for _, r := range []struct{ by, text string }{{"jonas", "<p>Noon?</p>"}, {"maya", "<p>Noon works</p>"}, {"jonas", "<p>See you</p>"}} {
			if code, _, msg := w.call(t, r.by, chatController.CreateChatComment, http.MethodPost, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: created.UUID, HTMLText: r.text}); code != http.StatusOK && code != http.StatusCreated {
				t.Fatalf("%s's reply: %d %s", r.by, code, msg)
			}
		}
		code, data, msg := w.call(t, "maya", chatController.GetLatestChats, http.MethodGet, nil, "user_uuid", w.people["jonas"].String())
		if code != http.StatusOK {
			t.Fatalf("the page: %d %s", code, msg)
		}
		var page struct {
			Chats []struct {
				UUID     string      `json:"chat_uuid"`
				Comments []pageReply `json:"chat_comments"`
			} `json:"chats"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Chats {
			if c.UUID != created.UUID {
				continue
			}
			var who []string
			for _, r := range c.Comments {
				who = append(who, r.By.Name)
				if r.By.Email != "" || r.Text != "" {
					t.Errorf("a face carries %q, %q", r.By.Email, r.Text)
				}
			}
			if !slices.Equal(who, []string{"maya", "jonas"}) {
				t.Fatalf("faces %v: want Maya, then Jonas, who replied last", who)
			}
			return
		}
		t.Fatalf("the message isn't on its page: %s", data)
	})
}
