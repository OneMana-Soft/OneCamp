//go:build integration
// +build integration

package integration_test

// A bot's reply in a thread is told as a person's reply is, against real
// Postgres and Dgraph (V-574).
//
// An AI coworker's or agent's reply on your post (business/BotPost: the plain
// reply and the streamed one) was published to the channel only. It never went
// through the one notifier a person's reply goes through (business/Comment
// NotifyThreadReply), so its author had no push, no email and no live Activity
// item: GET /activity/unread counted the reply at once, but the page's badge
// rose only on a reload.
//
// Now each of them is told once, through that notifier: the plain reply when
// it is written, the streamed one when it is finished (never per word, never
// when it is dropped). Its email follows each person's settings, and the
// public demo's shared visitor is never emailed (helpers.IsDemoPerson): every
// AI reply on the demo would otherwise spend the shared free email allowance.
//
// Pushes are watched where every path sends them, and emails are read from the
// queue they wait in, so this runs without Firebase or an email provider.
//
// Run: go test -tags=integration ./tests/integration/ -run TestABotsThreadReplyIsToldAsAPersonsIs -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	activityController "github.com/akashc777/OneCamp/controllers/Activity"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestABotsThreadReplyIsToldAsAPersonsIs(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("DEMO_MODE", "")
	t.Setenv("DEMO_USER_EMAIL", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	mq := integration.RecordMqtt(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// #launch is private. bo wrote four posts there and asked the AI coworker
	// on each; zed is in the channel, and replied on the fourth before.
	people := map[string]uuid.UUID{}
	for _, name := range []string{"bo", "zed"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@acme.test", name, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "tok-"+name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, id, "unsub-"+name)
	}
	channel := uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', true)`, channel)
	for _, name := range []string{"bo", "zed"} {
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, 'all')`,
			people[name], channel)
	}
	posts := map[string]uuid.UUID{}
	zero := "0001-01-01T00:00:00Z"
	var postNodes []map[string]any
	for _, key := range []string{"plain", "streamed", "dropped", "demo"} {
		posts[key] = uuid.New()
		exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, posts[key], channel, people["bo"])
		node := map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": posts[key].String(),
			"post_text": "<p>@OneCamp AI what are the numbers for " + key + "?</p>", "post_deleted_at": zero,
			"post_created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			"post_by":         map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"}}
		if key == "demo" {
			node["post_comments"] = []map[string]any{{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(),
				"comment_text": "<p>Following</p>", "comment_deleted_at": zero, "comment_by": map[string]any{"uid": "_:zed"},
				"comment_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
		}
		postNodes = append(postNodes, node)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": people["bo"].String(), "user_name": "bo"},
		{"uid": "_:zed", "dgraph.type": "User", "user_uuid": people["zed"].String(), "user_name": "zed"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:bo"}, {"uid": "_:zed"}}, "ch_posts": postNodes},
	})

	bot, err := userBusiness.EnsureAgentBot(ctx, uuid.New(), "OneCamp AI", "")
	if err != nil || bot == nil {
		t.Fatalf("the coworker's principal: %v", err)
	}

	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	// The Inbox badge, as GET /activity/unread answers it.
	unread := func(name string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		activityController.GetActivityUnread(rec, httptest.NewRequest(http.MethodGet, "/activity/unread", nil).WithContext(as(name)))
		var body struct {
			Data struct {
				Count int `json:"count"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("%s's badge: %d %s", name, rec.Code, rec.Body.String())
		}
		return body.Data.Count
	}
	// The live Activity items of a kind sent to name about the post's thread.
	told := func(name, kind, post string) int {
		n := 0
		for _, raw := range mq.Messages(helpers.GetMqttTopicForUserActivity(people[name].String()), int8(mqttStruct.MESSAGE_ACTIVITY)) {
			var m struct {
				Activity struct {
					Type string `json:"activity_type"`
				} `json:"activity"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Activity.Type == kind && strings.Contains(string(raw), post) {
				n++
			}
		}
		return n
	}
	anyMention := func() bool {
		for _, p := range mq.All() {
			if strings.Contains(string(p.Payload), `"activity_type":"`+mqttStruct.MESSAGE_ACTIVITY_MENTION+`"`) {
				return true
			}
		}
		return false
	}
	// The pushes about the post's thread that reached name's phone.
	pushed := func(name, post string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, p := range pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT &&
				p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == post && slices.Contains(p.tokens, "tok-"+name) {
				n++
			}
		}
		return n
	}
	emailed := func(name string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE event_type = 'post.comment' AND user_id = $1`,
			people[name]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	latest := func(post string) time.Time {
		t.Helper()
		at, err := threadIndex.LatestReply(ctx, post)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	settle := func() { time.Sleep(700 * time.Millisecond) } // anything more would be in by now

	badge := unread("bo")

	t.Run("a plain reply is told to the post's author once", func(t *testing.T) {
		post := posts["plain"].String()
		if _, err := botpost.PostCommentToPostAsBot(ctx, posts["plain"], "The numbers are in the sheet.", bot); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "bo's live Activity item for the coworker's reply", func() bool { return told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) > 0 })
		waitFor(t, "bo's push for the coworker's reply", func() bool { return pushed("bo", post) > 0 })
		waitFor(t, "bo's email for the coworker's reply", func() bool { return emailed("bo") > 0 })
		settle()
		if n := told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post); n != 1 {
			t.Errorf("bo was sent %d live Activity items for the reply; want 1", n)
		}
		if n := pushed("bo", post); n != 1 {
			t.Errorf("bo was pushed %d times for the reply; want 1", n)
		}
		if n := emailed("bo"); n != 1 {
			t.Errorf("bo was emailed %d times for the reply; want 1, by his settings", n)
		}
		if n := told("zed", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) + pushed("zed", post); n != 0 {
			t.Errorf("zed, in the channel but not the thread, was told %d times; want 0", n)
		}
		if anyMention() {
			t.Error("a Mention was published; the reply mentions nobody")
		}
		if latest(post).IsZero() {
			t.Error("the thread's index has no reply: the Threads view would miss it")
		}
		if got := unread("bo"); got != badge+1 {
			t.Errorf("bo's badge reads %d; want %d, the reply counted once", got, badge+1)
		}
	})

	t.Run("a streamed reply is told once, when it is finished", func(t *testing.T) {
		post := posts["streamed"].String()
		stream, err := botpost.BeginPostCommentStream(ctx, posts["streamed"], bot, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, words := range []string{"The numbers", "The numbers are in", "The numbers are in the sheet"} {
			stream.Push(ctx, words)
			time.Sleep(300 * time.Millisecond) // past the live edit's throttle, so each Push is published
		}
		if n := told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) + pushed("bo", post); n != 0 {
			t.Errorf("bo was told %d times while the reply was still being written; want 0 until it is finished", n)
		}
		if _, err := stream.Finalize(ctx, "The numbers are in the sheet, row 4."); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "bo's live Activity item for the streamed reply", func() bool { return told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) > 0 })
		waitFor(t, "bo's push for the streamed reply", func() bool { return pushed("bo", post) > 0 })
		settle()
		if n := told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post); n != 1 {
			t.Errorf("bo was sent %d live Activity items for a reply streamed in three Pushes; want 1", n)
		}
		if n := pushed("bo", post); n != 1 {
			t.Errorf("bo was pushed %d times for the streamed reply; want 1", n)
		}
		if n := emailed("bo"); n != 2 {
			t.Errorf("bo has %d reply emails; want 2, one for each finished reply", n)
		}
		if latest(post).IsZero() {
			t.Error("the thread's index has no reply for the streamed one")
		}
		if got := unread("bo"); got != badge+2 {
			t.Errorf("bo's badge reads %d; want %d, the streamed reply counted once", got, badge+2)
		}
	})

	t.Run("a dropped stream tells nobody", func(t *testing.T) {
		post := posts["dropped"].String()
		stream, err := botpost.BeginPostCommentStream(ctx, posts["dropped"], bot, "")
		if err != nil {
			t.Fatal(err)
		}
		stream.Push(ctx, "The numbers")
		stream.Abort(ctx)
		settle()
		if n := told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) + pushed("bo", post); n != 0 {
			t.Errorf("bo was told %d times of a reply that was dropped; want 0", n)
		}
		if at := latest(post); !at.IsZero() {
			t.Errorf("the thread's index has a reply at %v that was dropped; want none", at)
		}
	})

	t.Run("on the demo the shared visitor is told live but never emailed", func(t *testing.T) {
		// bo is the demo's shared visitor: everyone who opens the demo is him.
		t.Setenv("DEMO_MODE", "true")
		t.Setenv("DEMO_USER_EMAIL", "bo@acme.test")
		post := posts["demo"].String()
		before := emailed("bo")
		if _, err := botpost.PostCommentToPostAsBot(ctx, posts["demo"], "The numbers are in the sheet.", bot); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the visitor's live Activity item", func() bool { return told("bo", mqttStruct.MESSAGE_ACTIVITY_COMMENT, post) > 0 })
		waitFor(t, "zed's email, who replied before", func() bool { return emailed("zed") > 0 })
		settle()
		if n := emailed("bo") - before; n != 0 {
			t.Errorf("the demo's shared visitor was emailed %d times for an AI reply; want 0 (one per reply would spend the free 100 a day)", n)
		}
		if n := pushed("bo", post); n != 1 {
			t.Errorf("the visitor was pushed %d times; want 1", n)
		}
	})
}
