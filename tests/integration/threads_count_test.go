//go:build integration
// +build integration

package integration_test

// The Threads count is the Threads list's (NOTIF-H4): it counts exactly the
// unread threads the list shows, so a thread in a conversation the person
// left or lost, or whose first message is deleted, is neither listed nor
// counted, and replies there raise nothing. And what that costs a badge read
// at 500 followed unread threads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

func TestTheThreadsCountIsTheThreadsListed(t *testing.T) {
	w := newB25World(t)
	ctx := context.Background()

	// Maya wrote three messages: one in #launch that stays, one in #launch
	// whose message is deleted, and one in #inner, private, which she leaves.
	launch, inner := uuid.NewString(), uuid.NewString()
	kept, gone, left := uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas"}, nil, map[string]string{kept: "maya", gone: "maya"})
	w.channel(t, inner, "inner", true, "", []string{"maya", "jonas"}, nil, map[string]string{left: "maya"})
	for id, name := range map[string]string{launch: "launch", inner: "inner"} {
		if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, id, name, id == inner); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(name, post string) {
		t.Helper()
		code, data, msg := w.call(t, name, postController.CreateCommentInPost, http.MethodPost,
			postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: "<p>a reply</p>"})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("%s replying: %d %s %s", name, code, data, msg)
		}
	}
	// lookup is the uid of the node whose pred is value.
	lookup := func(pred, value string) string {
		t.Helper()
		var got struct {
			Q []struct {
				UID string `json:"uid"`
			} `json:"q"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(%s, %q)) { uid } }`, pred, value)), &got); err != nil || len(got.Q) != 1 {
			t.Fatalf("no node with %s %s: %v", pred, value, err)
		}
		return got.Q[0].UID
	}
	mutate := func(m *api.Mutation) {
		t.Helper()
		m.CommitNow = true
		if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	// both is Maya's Threads list (its rows and its unread) and her count.
	both := func(t *testing.T) (listed []string, unread, count int) {
		t.Helper()
		code, data, msg := w.call(t, "maya", conversationController.GetMyThreads, http.MethodGet, nil)
		var page threadsPage
		if code != http.StatusOK || json.Unmarshal(data, &page) != nil {
			t.Fatalf("maya's threads: %d %s %s", code, data, msg)
		}
		for _, th := range page.Threads {
			name := map[string]string{kept: "kept", gone: "gone", left: "left"}[th.Root]
			if th.Unread {
				name += " (unread)"
			}
			listed = append(listed, name)
		}
		slices.Sort(listed)
		code, data, msg = w.call(t, "maya", conversationController.GetThreadsUnread, http.MethodGet, nil)
		var c struct {
			Unread int `json:"unread"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &c) != nil {
			t.Fatalf("maya's count: %d %s %s", code, data, msg)
		}
		return listed, page.Unread, c.Unread
	}

	for _, root := range []string{kept, gone, left} {
		reply("jonas", root)
	}
	eventually(t, "maya's three threads", func() bool { _, _, n := both(t); return n == 3 })
	if listed, unread, count := both(t); len(listed) != 3 || unread != 3 || count != 3 {
		t.Fatalf("before anything goes: listed %v, the list's unread %d, the count %d; want all three, 3 and 3", listed, unread, count)
	}

	// She leaves #inner (private: she can't read it now), and her message
	// in #launch is deleted.
	mutate(&api.Mutation{DelNquads: []byte(fmt.Sprintf("<%s> <ch_members> <%s> .", lookup("ch_uuid", inner), w.uids["maya"]))})
	mutate(&api.Mutation{SetNquads: []byte(fmt.Sprintf("<%s> <post_deleted_at> %q .", lookup("post_uuid", gone), time.Now().UTC().Format(time.RFC3339)))})

	t.Run("a thread she can't read or whose message is gone is neither listed nor counted", func(t *testing.T) {
		listed, unread, count := both(t)
		if !slices.Equal(listed, []string{"kept (unread)"}) || unread != 1 || count != 1 {
			t.Errorf("listed %v, the list's unread %d, the count %d; want [kept (unread)], 1 and 1", listed, unread, count)
		}
	})
	t.Run("a reply in the conversation she left raises neither", func(t *testing.T) {
		reply("jonas", left)
		time.Sleep(time.Second)
		listed, unread, count := both(t)
		if !slices.Equal(listed, []string{"kept (unread)"}) || unread != 1 || count != 1 {
			t.Errorf("listed %v, the list's unread %d, the count %d; want [kept (unread)], 1 and 1", listed, unread, count)
		}
	})
}

// TestThreadsCountCostUpTo500UnreadThreads times GET /chat-threads/unread for
// someone following 10, 50 and then 500 unread threads (500 is the list's
// cap), median of 5 at each: the sidebar reads it often. The threads are
// added as it goes, in one world.
func TestThreadsCountCostUpTo500UnreadThreads(t *testing.T) {
	w := newB25World(t)
	launch := uuid.NewString()
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, launch); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		code, data, msg := w.call(t, "maya", conversationController.GetThreadsUnread, http.MethodGet, nil)
		var c struct {
			Unread int `json:"unread"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &c) != nil {
			t.Fatalf("maya's count: %d %s %s", code, data, msg)
		}
		return c.Unread
	}
	// The roots are there from the start; the threads on them are added as
	// it goes.
	const most = 500
	posts := make(map[string]string, most)
	roots := make([]string, 0, most)
	for range most {
		id := uuid.NewString()
		posts[id] = "maya"
		roots = append(roots, id)
	}
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas"}, nil, posts)
	made := 0
	for _, n := range []int{10, 50, most} {
		// Each a thread of Maya's with a reply since she read it.
		for i := made; i < n; i++ {
			at := time.Now().Add(-time.Duration(i) * time.Second)
			if _, err := w.env.PG.Exec(`INSERT INTO threads (id, resource_kind, resource_uuid, created_by, last_reply_at) VALUES ($1, 'channel', $2, $3, $4)`,
				roots[i], launch, w.people["jonas"], at); err != nil {
				t.Fatal(err)
			}
			if _, err := w.env.PG.Exec(`INSERT INTO thread_participants (thread_id, user_uuid, role) VALUES ($1, $2, 'author')`, roots[i], w.people["maya"]); err != nil {
				t.Fatal(err)
			}
		}
		made = n
		if got := count(); got != n {
			t.Fatalf("the count at %d unread threads: %d", n, got)
		}
		took := make([]time.Duration, 0, 5)
		for range 5 {
			start := time.Now()
			count()
			took = append(took, time.Since(start))
		}
		slices.Sort(took)
		t.Logf("threads count at %d unread threads: median of 5 %.1f ms (min %.1f, max %.1f)", n,
			float64(took[2].Microseconds())/1000, float64(took[0].Microseconds())/1000, float64(took[4].Microseconds())/1000)
	}
}
