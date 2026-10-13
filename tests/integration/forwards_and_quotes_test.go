//go:build integration
// +build integration

package integration_test

// A forward and a reply quote show what they point at on every page of a
// channel, and a forwarded message deleted since shows as deleted wherever it
// was forwarded, against a real Postgres and Dgraph.
//
// Only the latest page selected forwards and reply quotes, so beyond the
// newest ten messages a forward showed without what it forwarded and a reply
// without its quote (BE-BUG-3). And a forward is an edge to the original, read
// with no deletion check: a deleted message stayed readable, text, author and
// channel, wherever it had been forwarded, and a deleted message could be
// forwarded back into view (BE-BUG-8).
//
// Run: go test -tags=integration ./tests/integration/ -run TestForwardsAndQuotesOnEveryPage -v

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
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestForwardsAndQuotesOnEveryPage(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })

	sam, maya := uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{sam: "sam", maya: "maya"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	general, launch := uuid.New(), uuid.New()
	for ch, name := range map[uuid.UUID]string{general: "general", launch: "launch"} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, ch, name); err != nil {
			t.Fatal(err)
		}
	}
	original := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, original, launch, maya); err != nil {
		t.Fatal(err)
	}

	const live = "0001-01-01T00:00:00Z"
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	at := func(s int) string { return base.Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano) }
	members := []map[string]any{{"uid": "_:sam"}, {"uid": "_:maya"}}
	post := func(key, channel, by, text string, s int, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": text,
			"post_by": map[string]any{"uid": "_:" + by}, "post_channel": map[string]any{"uid": "_:" + channel},
			"post_created_at": at(s), "post_deleted_at": live}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	// In #launch, Maya's message. In #general: Sam's forward of it, a quote
	// target, a reply quoting it, then eleven more messages, so the forward
	// and the reply are past the newest ten.
	generalPosts := []map[string]any{
		post("forward", "general", "sam", "", 10, map[string]any{"post_fwd_msg_post": map[string]any{"uid": "_:original"}}),
		post("quoted", "general", "maya", "<p>which deck?</p>", 20, nil),
		post("reply", "general", "sam", "<p>the blue one</p>", 30, map[string]any{"post_reply_to": map[string]any{"uid": "_:quoted"}}),
	}
	for i := 0; i < 11; i++ {
		generalPosts = append(generalPosts, post(fmt.Sprintf("later%d", i), "general", "maya", fmt.Sprintf("<p>later %d</p>", i), 100+i, nil))
	}
	nodes := []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": live, "ch_members": members,
			"ch_posts": []map[string]any{func() map[string]any {
				p := post("original", "launch", "maya", "<p>the launch password is hunter2</p>", 0, nil)
				p["post_uuid"] = original.String()
				return p
			}()}},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_private": false,
			"ch_deleted_at": live, "ch_members": members, "ch_posts": generalPosts},
	}
	// A DM where Sam forwarded Maya's DM message.
	grp := helpers.GetGroupingId(sam.String(), maya.String())
	nodes = append(nodes, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": grp, "dm_participants": members,
		"dm_chats": []map[string]any{
			{"uid": "_:dmOriginal", "dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "<p>my salary is 90k</p>",
				"chat_from": map[string]any{"uid": "_:maya"}, "chat_created_at": at(0), "chat_deleted_at": live},
			{"uid": "_:dmForward", "dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "",
				"chat_from": map[string]any{"uid": "_:sam"}, "chat_created_at": at(10), "chat_deleted_at": live,
				"chat_fwd_msg_chat": map[string]any{"uid": "_:dmOriginal"}},
		}})
	uids := dg.Mutate(t, nodes)

	byUUID := func(posts []*dgraphStruct.DgraphPost) map[string]*dgraphStruct.DgraphPost {
		m := map[string]*dgraphStruct.DgraphPost{}
		for _, p := range posts {
			m[p.Uuid] = p
		}
		return m
	}
	uuidOf := func(key string) string {
		for _, p := range generalPosts {
			if p["uid"] == "_:"+key {
				return p["post_uuid"].(string)
			}
		}
		t.Fatalf("no post %s", key)
		return ""
	}
	forwardID, replyID := uuidOf("forward"), uuidOf("reply")

	// pages are the three ways past the newest ten: the page before the
	// eleventh-newest message, the page after the start, and the page
	// around the forward (a link to it).
	pages := func(t *testing.T) map[string]map[string]*dgraphStruct.DgraphPost {
		t.Helper()
		older, err := postDomain.GetDgraphOldPostFromDgraph(ctx, general.String(), base.Add(100*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		newer, err := postDomain.GetDgraphNewPostFromDgraph(ctx, general.String(), base.Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		forwardAt := base.Add(10 * time.Second)
		around, err := postDomain.GetDgraphNewPostIncludiongPostFromDgraph(ctx, general.String(), &forwardAt)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]map[string]*dgraphStruct.DgraphPost{"older": byUUID(older), "newer": byUUID(newer), "around": byUUID(around)}
	}

	t.Run("past the newest ten, a forward shows what it forwarded and a reply its quote", func(t *testing.T) {
		latest, err := postDomain.GetDgraphLatestPostFromDgraph(ctx, general.String())
		if err != nil {
			t.Fatal(err)
		}
		if byUUID(latest)[forwardID] != nil {
			t.Fatal("the fixture is wrong: the forward is on the latest page")
		}
		for name, page := range pages(t) {
			f, r := page[forwardID], page[replyID]
			if f == nil || r == nil {
				t.Fatalf("the %s page doesn't hold the forward and the reply", name)
			}
			if f.ForwaredPost == nil || f.ForwaredPost.Text != "<p>the launch password is hunter2</p>" || f.ForwaredPost.PostBy == nil {
				t.Errorf("on the %s page the forward shows %+v, want Maya's message", name, f.ForwaredPost)
			}
			if r.ReplyToPost == nil || r.ReplyToPost.Text != "<p>which deck?</p>" {
				t.Errorf("on the %s page the reply quotes %+v, want the message it replies to", name, r.ReplyToPost)
			}
		}
	})

	// Maya deletes her messages, as DeletePost and DeleteChat do: the time.
	deleted := time.Now().UTC().Format(time.RFC3339Nano)
	dg.Mutate(t, []map[string]any{
		{"uid": uids["original"], "post_deleted_at": deleted},
		{"uid": uids["dmOriginal"], "chat_deleted_at": deleted},
	})
	gone := func(t *testing.T, where string, p *dgraphStruct.DgraphPost) {
		t.Helper()
		if p == nil {
			t.Errorf("%s: the forward lost its original altogether; it should show as deleted", where)
			return
		}
		if p.Text != "" || p.PostBy != nil || p.Channel != nil || !helpers.IsSoftDeleted(p.DeletedAt) {
			t.Errorf("%s: a deleted original shows %+v; want only that it was deleted", where, p)
		}
	}

	t.Run("a deleted message shows as deleted wherever it was forwarded", func(t *testing.T) {
		for name, page := range pages(t) {
			gone(t, "the "+name+" page", page[forwardID].ForwaredPost)
		}
		thread, err := postDomain.GetDgraphPostByUUIDWithAllComments(ctx, forwardID, uids["sam"])
		if err != nil {
			t.Fatal(err)
		}
		gone(t, "the forward's thread", thread.ForwaredPost)

		chats, err := chatDomain.GetDgraphLatestChatFromDgraph(ctx, grp)
		if err != nil {
			t.Fatal(err)
		}
		var fwd *dgraphStruct.DgraphChat
		for _, c := range chats {
			if c.FwdMsgChat != nil {
				fwd = c.FwdMsgChat
			}
		}
		if fwd == nil || fwd.Body != "" || fwd.From != nil || !helpers.IsSoftDeleted(fwd.DeletedAt) {
			t.Errorf("in the DM, a deleted original shows %+v; want only that it was deleted", fwd)
		}
	})

	t.Run("a deleted message can't be forwarded back into view", func(t *testing.T) {
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
		if err != nil || pg == nil {
			t.Fatalf("load sam: %v", err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
		if err != nil || gr == nil {
			t.Fatalf("load sam's node: %v", err)
		}
		asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
		raw, _ := json.Marshal(userAdapter.UserFwdMsgInput{
			ChannelUuid: launch.String(), PostUuid: original.String(),
			FwdTo: []userAdapter.UserAndChannelFwdMessage{{ChannelDgraphUid: uids["general"]}},
		})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asSam))
		if rec.Code != http.StatusNotFound {
			t.Errorf("forwarding a deleted message answered %d %s, want 404", rec.Code, rec.Body.String())
		}
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, general).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("forwarding a deleted message wrote %d posts", n)
		}
	})
}
