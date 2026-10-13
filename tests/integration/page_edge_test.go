//go:build integration
// +build integration

package integration_test

// Does paging a conversation ever skip a message?
//
// Pages were asked for in whole seconds (the floor of the oldest message's
// time for an older page, the ceiling of the newest's for a newer one), and
// compared strictly against nanosecond times, so a message sharing a second
// with a page's edge was in neither page. The app now names the edge message
// itself (?at= its exact time, ?id= its id); this asks the handlers the way
// the app does, from a page the server gave, and checks nothing is missing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPaging -v

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type pageAnswer struct {
	Data struct {
		Posts []struct {
			UUID      string `json:"post_uuid"`
			CreatedAt string `json:"post_created_at"`
		} `json:"posts"`
		Chats []struct {
			UUID      string `json:"chat_uuid"`
			CreatedAt string `json:"chat_created_at"`
		} `json:"chats"`
		HasMore bool `json:"has_more"`
	} `json:"data"`
}

func TestPagingNeverSkipsAMessageInTheSameSecondAsTheEdge(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	reader, channel := uuid.NewString(), uuid.NewString()
	second := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return second.Add(d).Format(time.RFC3339Nano) }
	// Three posts in one second (a bot's burst), the oldest of the channel,
	// and nine after them: the newest page of ten ends inside that second.
	posts := []map[string]any{
		{"uid": "_:p1", "post_uuid": uuid.NewString(), "post_text": "1", "post_created_at": at(100 * time.Millisecond)},
		{"uid": "_:p2", "post_uuid": uuid.NewString(), "post_text": "2", "post_created_at": at(200 * time.Millisecond)},
		{"uid": "_:p3", "post_uuid": uuid.NewString(), "post_text": "3", "post_created_at": at(300 * time.Millisecond)},
	}
	for i := 1; i <= 9; i++ {
		posts = append(posts, map[string]any{"uid": "_:q" + strconv.Itoa(i), "post_uuid": uuid.NewString(),
			"post_text": "later " + strconv.Itoa(i), "post_created_at": at(time.Duration(i) * time.Second)})
	}
	var refs []map[string]any
	for _, p := range posts {
		refs = append(refs, map[string]any{"uid": p["uid"]})
	}
	nodes := append([]map[string]any{
		{"uid": "_:reader", "user_uuid": reader, "user_name": "reader", "user_channels": []map[string]any{{"uid": "_:ch"}}},
		{"uid": "_:ch", "ch_uuid": channel, "ch_name": "bots", "ch_private": false,
			"ch_members": []map[string]any{{"uid": "_:reader"}}, "ch_posts": refs},
	}, posts...)
	uids := dg.Mutate(t, nodes)

	call := func(handler http.HandlerFunc, params map[string]string, query string) pageAnswer {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		var who userModels.UserInfo
		who.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["reader"], Uuid: reader}
		reqCtx := context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rctx), helpers.UserInfoContextKey, who)
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(reqCtx))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var answer pageAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}

	latest := call(postController.GetLatestPosts, map[string]string{"channel_uuid": channel}, "")
	if len(latest.Data.Posts) != 10 {
		t.Fatalf("the newest page has %d posts, want 10", len(latest.Data.Posts))
	}
	edge := latest.Data.Posts[len(latest.Data.Posts)-1] // the oldest on screen: post 3
	edgeTime, err := time.Parse(time.RFC3339Nano, edge.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// As the app asks: whole seconds in the path (for apps from before), the
	// edge message itself in the query.
	older := call(postController.GetOldPosts,
		map[string]string{"channel_uuid": channel, "time_stamp": strconv.FormatInt(edgeTime.Unix(), 10)},
		"at="+url.QueryEscape(edge.CreatedAt)+"&id="+edge.UUID)
	seen := map[string]bool{}
	for _, p := range append(latest.Data.Posts, older.Data.Posts...) {
		if seen[p.UUID] {
			t.Errorf("post %s came back twice", p.UUID)
		}
		seen[p.UUID] = true
	}
	for _, p := range posts {
		if !seen[p["post_uuid"].(string)] {
			t.Errorf("post %q, in the same second as the page's edge, never came back", p["post_text"])
		}
	}
	if older.Data.HasMore {
		t.Error("the older page says there's more before the channel's first post")
	}

	// And the other way, from a link to post 1: the newer pages.
	first := posts[0]
	firstTime, _ := time.Parse(time.RFC3339Nano, first["post_created_at"].(string))
	newer := call(postController.GetNewPosts,
		map[string]string{"channel_uuid": channel, "time_stamp": strconv.FormatInt(int64(math.Ceil(float64(firstTime.UnixNano())/1e9)), 10)},
		"at="+url.QueryEscape(first["post_created_at"].(string))+"&id="+first["post_uuid"].(string))
	got := map[string]bool{}
	for _, p := range newer.Data.Posts {
		got[p.UUID] = true
	}
	for _, p := range posts[1:3] {
		if !got[p["post_uuid"].(string)] {
			t.Errorf("post %q, in the same second as the page's edge, is missing from the newer page", p["post_text"])
		}
	}
	if got[first["post_uuid"].(string)] {
		t.Error("the newer page repeats its edge")
	}
}

func TestPagingADMNeverSkipsAMessageInTheSameSecondAsTheEdge(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me, them := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{me, them} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, 'p', NOW(), NOW())`, id, id.String()+"@example.test", "p-"+id.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	second := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return second.Add(d).Format(time.RFC3339Nano) }
	chats := []map[string]any{
		{"uid": "_:c1", "chat_uuid": uuid.NewString(), "chat_body_text": "1", "chat_created_at": at(100 * time.Millisecond)},
		{"uid": "_:c2", "chat_uuid": uuid.NewString(), "chat_body_text": "2", "chat_created_at": at(200 * time.Millisecond)},
		{"uid": "_:c3", "chat_uuid": uuid.NewString(), "chat_body_text": "3", "chat_created_at": at(300 * time.Millisecond)},
	}
	var refs []map[string]any
	for _, c := range chats {
		refs = append(refs, map[string]any{"uid": c["uid"]})
	}
	uids := dg.Mutate(t, append([]map[string]any{
		{"uid": "_:me", "user_uuid": me.String(), "user_name": "me"},
		{"uid": "_:them", "user_uuid": them.String(), "user_name": "them"},
		{"uid": "_:dm", "dm_grouping_id": helpers.GetGroupingId(me.String(), them.String()),
			"dm_participants": []map[string]any{{"uid": "_:me"}, {"uid": "_:them"}}, "dm_chats": refs},
	}, chats...))

	req := httptest.NewRequest(http.MethodGet, "/?at="+url.QueryEscape(chats[2]["chat_created_at"].(string))+"&id="+chats[2]["chat_uuid"].(string), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("user_uuid", them.String())
	rctx.URLParams.Add("time_stamp", strconv.FormatInt(second.Unix(), 10))
	var who userModels.UserInfo
	who.UserPostgresInfo.Id = me
	who.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["me"], Uuid: me.String()}
	rec := httptest.NewRecorder()
	chatController.GetOldChats(rec, req.WithContext(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rctx), helpers.UserInfoContextKey, who)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var answer pageAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range answer.Data.Chats {
		got[c.UUID] = true
	}
	for _, c := range chats[:2] {
		if !got[c["chat_uuid"].(string)] {
			t.Errorf("message %q, in the same second as the page's edge, never came back", c["chat_body_text"])
		}
	}
	if got[chats[2]["chat_uuid"].(string)] {
		t.Error("the older page repeats its edge")
	}
}
