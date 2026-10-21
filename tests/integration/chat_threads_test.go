//go:build integration
// +build integration

package integration_test

// Threads you follow (chat P2), against real Postgres 12, Redis and Dgraph,
// through the handlers the app calls: a reply puts its thread in the Threads
// view of everyone taking part, unread for all but its writer; following by
// hand adds it, turning it off takes it out and stops its notices; reading
// it, marking it unread and marking all read; refusals in words; and the
// sweep that adopts threads from before replies were indexed, read.
//
// Run: go test -tags=integration ./tests/integration/ -run TestThreadsYouFollow -v

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatThreadBusiness "github.com/akashc777/OneCamp/business/ChatThread"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
)

// threadsPage is GET /chat-threads's data.
type threadsPage struct {
	Threads []struct {
		Root         string `json:"root"`
		Conversation struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"conversation"`
		Place struct {
			Name   string `json:"name"`
			People []struct {
				UUID string `json:"user_uuid"`
			} `json:"people"`
			Count int `json:"count"`
		} `json:"place"`
		Message struct {
			HTML string `json:"html"`
			By   *struct {
				UUID string `json:"user_uuid"`
			} `json:"by"`
		} `json:"message"`
		Replies int `json:"replies"`
		New     int `json:"new"`
		Latest  []struct {
			HTML string `json:"html"`
		} `json:"latest"`
		Unread bool   `json:"unread"`
		Follow string `json:"follow"`
	} `json:"threads"`
	Next   int `json:"next"`
	Unread int `json:"unread"`
}

func TestThreadsYouFollow(t *testing.T) {
	w := newB25World(t)
	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()
	// pushedTo says whether name was pushed a reply in thread since push number from.
	pushedSince := func(from int, name, thread string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range pushes[min(from, len(pushes)):] {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == thread && slices.Contains(p.tokens, "tok-"+name) {
				return true
			}
		}
		return false
	}

	launch, secret, plan, hidden := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas", "sam"}, nil, map[string]string{plan: "maya"})
	w.channel(t, secret, "secret", true, "", []string{"jonas"}, nil, map[string]string{hidden: "jonas"})
	// Who is in #launch, as a reply's notices read it: a setting each, from
	// joining.
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, launch); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"maya", "jonas", "sam"} {
		if _, err := w.env.PG.Exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, 'all')`, w.people[name], launch); err != nil {
			t.Fatal(err)
		}
	}

	mine := func(name string) threadsPage {
		t.Helper()
		code, data, msg := w.call(t, name, conversationController.GetMyThreads, http.MethodGet, nil)
		var page threadsPage
		if code != http.StatusOK || json.Unmarshal(data, &page) != nil {
			t.Fatalf("%s's threads: %d %s %s", name, code, data, msg)
		}
		return page
	}
	find := func(page threadsPage, root string) int {
		for i, th := range page.Threads {
			if th.Root == root {
				return i
			}
		}
		return -1
	}
	reply := func(name, post, html string) {
		t.Helper()
		code, data, msg := w.call(t, name, postController.CreateCommentInPost, http.MethodPost,
			postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: html})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("%s replying: %d %s %s", name, code, data, msg)
		}
	}
	follow := func(name, root, choice string) (int, string) {
		code, _, msg := w.call(t, name, conversationController.PutThreadFollow, http.MethodPut, map[string]string{"follow": choice}, "root", root)
		return code, msg
	}
	read := func(name, root string, read bool) int {
		code, _, _ := w.call(t, name, conversationController.PutThreadRead, http.MethodPut, map[string]bool{"read": read}, "root", root)
		return code
	}

	t.Run("a reply puts the thread in its people's threads, unread for all but its writer", func(t *testing.T) {
		reply("jonas", plan, "<p>Shipping Friday works</p>")
		eventually(t, "Maya's thread", func() bool { return find(mine("maya"), plan) >= 0 })
		page := mine("maya")
		th := page.Threads[find(page, plan)]
		if !th.Unread || th.New != 1 || th.Replies != 1 || page.Unread != 1 || th.Place.Name != "launch" ||
			th.Conversation.Kind != "channel" || th.Conversation.ID != launch || th.Follow != "auto" ||
			len(th.Latest) != 1 || !strings.Contains(th.Latest[0].HTML, "Shipping Friday") ||
			th.Message.By == nil || th.Message.By.UUID != w.people["maya"].String() {
			t.Fatalf("Maya's thread: %+v (unread %d)", th, page.Unread)
		}
		if page := mine("jonas"); find(page, plan) < 0 || page.Threads[find(page, plan)].Unread || page.Unread != 0 {
			t.Fatalf("Jonas, who wrote the reply: %+v", page)
		}
		if page := mine("sam"); find(page, plan) >= 0 {
			t.Fatalf("Sam took no part, and has it: %+v", page)
		}
		code, data, _ := w.call(t, "maya", conversationController.GetThreadsUnread, http.MethodGet, nil)
		var count struct {
			Unread int `json:"unread"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &count) != nil || count.Unread != 1 {
			t.Fatalf("Maya's count: %d %s", code, data)
		}
	})

	t.Run("reading it, marking it unread and marking all read", func(t *testing.T) {
		if code := read("maya", plan, true); code != http.StatusOK {
			t.Fatalf("reading: %d", code)
		}
		if page := mine("maya"); page.Unread != 0 || page.Threads[find(page, plan)].Unread {
			t.Fatalf("after reading: %+v", page)
		}
		if code := read("maya", plan, false); code != http.StatusOK {
			t.Fatalf("marking unread: %d", code)
		}
		if page := mine("maya"); page.Unread != 1 || !page.Threads[find(page, plan)].Unread {
			t.Fatalf("after marking unread: %+v", page)
		}
		code, data, _ := w.call(t, "maya", conversationController.PostThreadsReadAll, http.MethodPost, nil)
		var all struct {
			Read int `json:"read"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &all) != nil || all.Read != 1 || mine("maya").Unread != 0 {
			t.Fatalf("marking all read: %d %s", code, data)
		}
	})

	t.Run("following by hand brings later replies; turning it off stops them", func(t *testing.T) {
		if code, msg := follow("sam", plan, "on"); code != http.StatusOK {
			t.Fatalf("Sam following: %d %s", code, msg)
		}
		if page := mine("sam"); find(page, plan) < 0 || page.Threads[find(page, plan)].Unread {
			t.Fatalf("Sam, just following: %+v (the replies before are not new to him)", page)
		}
		if code, msg := follow("maya", plan, "off"); code != http.StatusOK {
			t.Fatalf("Maya turning her thread off: %d %s", code, msg)
		}
		if page := mine("maya"); find(page, plan) >= 0 {
			t.Fatalf("Maya turned it off, and has it: %+v", page)
		}
		mu.Lock()
		from := len(pushes)
		mu.Unlock()
		reply("jonas", plan, "<p>Booked the room</p>")
		eventually(t, "Sam's push", func() bool { return pushedSince(from, "sam", plan) })
		eventually(t, "Sam's unread", func() bool {
			page := mine("sam")
			i := find(page, plan)
			return i >= 0 && page.Threads[i].Unread && page.Threads[i].New == 1
		})
		if pushedSince(from, "maya", plan) {
			t.Fatal("Maya turned her thread off, and was pushed its reply")
		}
		code, data, _ := w.call(t, "maya", conversationController.GetThreadFollow, http.MethodGet, nil, "root", plan)
		var standing struct {
			Following bool   `json:"following"`
			Follow    string `json:"follow"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &standing) != nil || standing.Following || standing.Follow != "off" {
			t.Fatalf("Maya's standing: %d %s", code, data)
		}
	})

	t.Run("refused in words", func(t *testing.T) {
		if code, msg := follow("sam", hidden, "on"); code != http.StatusNotFound || msg != "That thread isn't there any more." {
			t.Fatalf("Sam following a private channel's thread: %d %q", code, msg)
		}
		if code := read("sam", hidden, true); code != http.StatusNotFound {
			t.Fatalf("Sam reading it: %d", code)
		}
		if code, msg := follow("sam", plan, "loud"); code != http.StatusBadRequest || msg == "" {
			t.Fatalf("a choice that isn't one: %d %q", code, msg)
		}
		if code, _ := follow("sam", "not-a-uuid", "on"); code != http.StatusNotFound {
			t.Fatalf("a root that isn't a uuid: %d", code)
		}
		if code := read("sam", uuid.NewString(), true); code != http.StatusNotFound {
			t.Fatalf("a root that isn't there: %d", code)
		}
	})

	t.Run("a DM's thread, with its people for its place", func(t *testing.T) {
		_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
		var created struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(data, &created) != nil || created.UUID == "" {
			t.Fatalf("the DM: %s", data)
		}
		code, data, msg := w.call(t, "maya", chatController.CreateChatComment, http.MethodPost, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: created.UUID, HTMLText: "<p>Noon</p>"})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("Maya's reply: %d %s %s", code, data, msg)
		}
		eventually(t, "Jonas's DM thread", func() bool { return find(mine("jonas"), created.UUID) >= 0 })
		page := mine("jonas")
		th := page.Threads[find(page, created.UUID)]
		if th.Conversation.Kind != "dm" || !th.Unread || th.Place.Count != 2 || len(th.Place.People) != 2 {
			t.Fatalf("Jonas's DM thread: %+v", th)
		}
	})

	t.Run("a thread from before replies were indexed is adopted, read", func(t *testing.T) {
		if _, err := w.env.PG.Exec(`DELETE FROM threads WHERE id = $1`, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := w.env.PG.Exec(`DELETE FROM system_configs WHERE key = 'chat.threads.adopt'`); err != nil {
			t.Fatal(err)
		}
		if page := mine("jonas"); find(page, plan) >= 0 {
			t.Fatalf("the thread is still there: %+v", page)
		}
		done := false
		for pass := 0; pass < 30 && !done; pass++ {
			report, err := chatThreadBusiness.AdoptEarlier(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			done = report.Done
		}
		if !done {
			t.Fatal("the sweep never finished")
		}
		page := mine("jonas")
		i := find(page, plan)
		if i < 0 || page.Threads[i].Unread || page.Threads[i].Replies != 2 {
			t.Fatalf("Jonas's adopted thread: %+v", page)
		}
		if page := mine("maya"); find(page, plan) < 0 || page.Threads[find(page, plan)].Unread {
			t.Fatalf("Maya's adopted thread, read: %+v", page)
		}
		// Done is done: a pass after it reads nothing.
		if report, err := chatThreadBusiness.AdoptEarlier(t.Context()); err != nil || !report.Done || report.Read != 0 {
			t.Fatalf("after done: %+v %v", report, err)
		}
	})
}
