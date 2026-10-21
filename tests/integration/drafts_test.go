//go:build integration
// +build integration

package integration_test

// Drafts kept on the server (chat P5), against real Postgres 12, Redis and
// Dgraph, through the handlers the app calls: a draft is kept where its writer
// can read, read back by them alone, emptied by a blank one or by Delete,
// kept until then for a conversation they have left, and refused in words for
// a place they can't read and past 500.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDraftsKeptOnTheServer -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// draftsAnswer is GET /drafts's data.
type draftsAnswer struct {
	Drafts []struct {
		Key      string `json:"key"`
		HTML     string `json:"html"`
		AlsoSend bool   `json:"also_send"`
		ReplyTo  *struct {
			UUID string `json:"uuid"`
			Text string `json:"text"`
		} `json:"reply_to"`
		Files []struct {
			Name string `json:"attachment_file_name"`
		} `json:"files"`
		UpdatedAt string `json:"updated_at"`
	} `json:"drafts"`
	Max int `json:"max"`
}

func TestDraftsKeptOnTheServer(t *testing.T) {
	w := newB25World(t)
	launch, secret, plan := uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas"}, nil, map[string]string{plan: "jonas"})
	w.channel(t, secret, "secret", true, "", []string{"jonas"}, nil, nil)

	put := func(name string, body map[string]any) (int, json.RawMessage, string) {
		return w.call(t, name, conversationController.PutDraft, http.MethodPut, body)
	}
	list := func(name string) draftsAnswer {
		t.Helper()
		code, data, msg := w.call(t, name, conversationController.GetDrafts, http.MethodGet, nil)
		var got draftsAnswer
		if code != http.StatusOK || json.Unmarshal(data, &got) != nil {
			t.Fatalf("%s's drafts: %d %s %s", name, code, data, msg)
		}
		return got
	}
	del := func(name, key string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/drafts?key="+url.QueryEscape(key), nil).WithContext(w.as(t, name))
		rec := httptest.NewRecorder()
		conversationController.DeleteDraft(rec, req)
		return rec.Code
	}
	channelKey, threadKey := "channel:"+launch, "post-thread:"+plan

	t.Run("a draft is kept, and read back by its writer alone", func(t *testing.T) {
		code, data, msg := put("maya", map[string]any{"key": channelKey, "html": "<p>Ship it Friday</p>",
			"reply_to": map[string]any{"uuid": plan, "author_name": "jonas", "text": "Ready?"},
			"files":    []map[string]any{{"attachment_uuid": uuid.NewString(), "attachment_file_name": "notes.pdf", "attachment_type": "document", "attachment_size": 2048}}})
		var kept struct {
			Key       string `json:"key"`
			UpdatedAt string `json:"updated_at"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &kept) != nil || kept.Key != channelKey || kept.UpdatedAt == "" {
			t.Fatalf("keeping it: %d %s %s", code, data, msg)
		}
		got := list("maya")
		if got.Max != 500 || len(got.Drafts) != 1 {
			t.Fatalf("Maya's drafts: %+v", got)
		}
		d := got.Drafts[0]
		if d.Key != channelKey || d.HTML != "<p>Ship it Friday</p>" || d.ReplyTo == nil || d.ReplyTo.Text != "Ready?" || len(d.Files) != 1 || d.Files[0].Name != "notes.pdf" {
			t.Fatalf("read back: %+v", d)
		}
		if others := list("jonas"); len(others.Drafts) != 0 {
			t.Fatalf("Jonas reads %+v", others.Drafts)
		}
	})

	t.Run("a second write takes the first's place", func(t *testing.T) {
		if code, _, msg := put("maya", map[string]any{"key": channelKey, "html": "<p>Ship it Monday</p>"}); code != http.StatusOK {
			t.Fatalf("%d %s", code, msg)
		}
		got := list("maya")
		if len(got.Drafts) != 1 || got.Drafts[0].HTML != "<p>Ship it Monday</p>" || got.Drafts[0].ReplyTo != nil {
			t.Fatalf("after the second write: %+v", got.Drafts)
		}
	})

	t.Run("a thread's draft, and a DM's", func(t *testing.T) {
		if code, _, msg := put("maya", map[string]any{"key": threadKey, "html": "<p>On it</p>", "also_send": true}); code != http.StatusOK {
			t.Fatalf("the thread's: %d %s", code, msg)
		}
		_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
		var created struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(data, &created) != nil || created.UUID == "" {
			t.Fatalf("the DM: %s", data)
		}
		dmKey := "dm:" + helpers.GetGroupingId(w.people["maya"].String(), w.people["jonas"].String())
		if code, _, msg := put("maya", map[string]any{"key": dmKey, "html": "<p>Yes, 12:30</p>"}); code != http.StatusOK {
			t.Fatalf("the DM's: %d %s", code, msg)
		}
		got := list("maya")
		keys := map[string]bool{}
		for _, d := range got.Drafts {
			keys[d.Key] = true
			if d.Key == threadKey && !d.AlsoSend {
				t.Fatalf("the thread's draft lost Also send: %+v", d)
			}
		}
		if len(got.Drafts) != 3 || !keys[channelKey] || !keys[threadKey] || !keys[dmKey] {
			t.Fatalf("Maya's drafts: %v", keys)
		}
	})

	t.Run("where its writer can't read, nothing is kept and the refusal says why", func(t *testing.T) {
		code, _, msg := put("maya", map[string]any{"key": "channel:" + secret, "html": "<p>psst</p>"})
		if code != http.StatusNotFound || msg != "That conversation isn't there any more, so its draft can't be kept." {
			t.Fatalf("a private channel she isn't in: %d %q", code, msg)
		}
		code, _, msg = put("maya", map[string]any{"key": "post-thread:" + uuid.NewString(), "html": "<p>psst</p>"})
		if code != http.StatusNotFound || msg != "That thread isn't there any more, so its draft can't be kept." {
			t.Fatalf("a thread that isn't there: %d %q", code, msg)
		}
		code, _, msg = put("maya", map[string]any{"key": "launch", "html": "<p>psst</p>"})
		if code != http.StatusBadRequest || msg != "That isn't a place a draft can be kept." {
			t.Fatalf("a key that names nothing: %d %q", code, msg)
		}
		if n := len(list("maya").Drafts); n != 3 {
			t.Fatalf("refusals kept something: %d drafts", n)
		}
	})

	t.Run("a blank draft empties its place, and so does Delete", func(t *testing.T) {
		if code, _, msg := put("maya", map[string]any{"key": threadKey, "html": "<p></p>"}); code != http.StatusOK {
			t.Fatalf("the blank one: %d %s", code, msg)
		}
		if code := del("maya", channelKey); code != http.StatusOK {
			t.Fatalf("Delete: %d", code)
		}
		got := list("maya")
		if len(got.Drafts) != 1 || got.Drafts[0].Key == channelKey || got.Drafts[0].Key == threadKey {
			t.Fatalf("after emptying two: %+v", got.Drafts)
		}
	})

	t.Run("a draft stays when its writer leaves the conversation, until they empty it", func(t *testing.T) {
		// A private channel: once she's out, she can't read it.
		plans := uuid.NewString()
		w.channel(t, plans, "plans", true, "", []string{"maya", "jonas"}, nil, nil)
		plansKey := "channel:" + plans
		if code, _, msg := put("maya", map[string]any{"key": plansKey, "html": "<p>Before I go</p>"}); code != http.StatusOK {
			t.Fatalf("%d %s", code, msg)
		}
		removeMember(t, w, plans, "maya")
		found := false
		for _, d := range list("maya").Drafts {
			found = found || (d.Key == plansKey && d.HTML == "<p>Before I go</p>")
		}
		if !found {
			t.Fatalf("leaving dropped her draft: %+v", list("maya").Drafts)
		}
		if code, _, _ := put("maya", map[string]any{"key": plansKey, "html": "<p>after</p>"}); code != http.StatusNotFound {
			t.Fatalf("a draft written after leaving: %d, want 404", code)
		}
		if code := del("maya", plansKey); code != http.StatusOK {
			t.Fatalf("emptying a left conversation's draft: %d", code)
		}
		for _, d := range list("maya").Drafts {
			if d.Key == plansKey {
				t.Fatalf("emptied, still kept: %+v", d)
			}
		}
	})

	t.Run("a person keeps up to 500", func(t *testing.T) {
		if _, err := w.env.PG.Exec(`INSERT INTO message_drafts (user_id, draft_key, body_html)
			SELECT $1, 'group:g' || n, '<p>x</p>' FROM generate_series(1, 499) AS n`, w.people["sam"]); err != nil {
			t.Fatal(err)
		}
		dmKey := "dm:" + helpers.GetGroupingId(w.people["sam"].String(), w.people["maya"].String())
		_, _, _ = w.call(t, "sam", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>hey</p>"})
		if code, _, msg := put("sam", map[string]any{"key": dmKey, "html": "<p>the 500th</p>"}); code != http.StatusOK {
			t.Fatalf("the 500th: %d %s", code, msg)
		}
		code, _, msg := put("sam", map[string]any{"key": "channel:" + launch, "html": "<p>the 501st</p>"})
		if code != http.StatusConflict || msg != "You have 500 drafts, the most that can be kept. Send or empty some first." {
			t.Fatalf("the 501st: %d %q", code, msg)
		}
		// One already kept still changes.
		if code, _, msg := put("sam", map[string]any{"key": dmKey, "html": "<p>changed</p>"}); code != http.StatusOK {
			t.Fatalf("changing one at the cap: %d %s", code, msg)
		}
	})
}

// removeMember takes name out of the channel's members in the graph.
func removeMember(t *testing.T, w *b25World, channelID, name string) {
	t.Helper()
	var got struct {
		C []struct {
			UID string `json:"uid"`
		}
	}
	q := `{ c(func: eq(ch_uuid, "` + channelID + `")) { uid } }`
	if err := json.Unmarshal(w.dg.Query(t, q), &got); err != nil || len(got.C) == 0 {
		t.Fatalf("the channel: %v", err)
	}
	nquad := fmt.Sprintf("<%s> <ch_members> <%s> .\n", got.C[0].UID, w.uids[name])
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(context.Background(), &api.Mutation{DelNquads: []byte(nquad), CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	// As a membership change does: the channel's cached standing goes with it.
	channelDomain.InvalidateChannelBasicInfo(context.Background(), channelID)
}
