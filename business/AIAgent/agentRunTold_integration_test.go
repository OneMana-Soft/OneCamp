//go:build integration

package business

// A durable run's reply in a channel thread is told once, with its answer,
// against real Postgres and Dgraph (V-574's review, CR-B15 row 2641 M1).
//
// The run shows one evolving status comment: "On it…" when it is slow, then
// its answer (or its question) as an edit. The status comment was written as a
// told reply, so the thread's people were pushed, emailed and given a live
// item saying "On it…", and the answer, an edit, told nobody. Now the ack is
// written untold and the first word that is not the ack is told, once.
//
// And one answer is one step of its triggerer's Inbox badge: when the thread's
// telling already put the answer in their Inbox (they wrote the post), the
// run's "replied" notice to them adds nothing; a triggerer the thread doesn't
// put it in front of still gets the notice.
//
// Run: go test -tags=integration ./business/AIAgent/ -run TestARunsReplyIsToldOnceWithItsAnswer -v

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

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARunsReplyIsToldOnceWithItsAnswer(t *testing.T) {
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

	type push struct {
		data   map[string]string
		tokens []string
	}
	var mu sync.Mutex
	var pushes []push
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, push{data, tokens})
		mu.Unlock()
	})()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// #launch is private. bo wrote three posts there and asked the agent on
	// the first two; on the third zed, who replied there before, asked it.
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
	for _, key := range []string{"slow", "fast", "zeds"} {
		posts[key] = uuid.New()
		exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, posts[key], channel, people["bo"])
		node := map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": posts[key].String(),
			"post_text": "<p>What are the numbers for " + key + "?</p>", "post_deleted_at": zero,
			"post_created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			"post_by":         map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"}}
		if key == "zeds" {
			node["post_comments"] = []map[string]any{{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(),
				"comment_text": "<p>@Release Captain can you check?</p>", "comment_deleted_at": zero, "comment_by": map[string]any{"uid": "_:zed"},
				"comment_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
		}
		postNodes = append(postNodes, node)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": people["bo"].String(), "user_name": "bo"},
		{"uid": "_:zed", "dgraph.type": "User", "user_uuid": people["zed"].String(), "user_name": "zed"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:bo"}, {"uid": "_:zed"}}, "ch_posts": postNodes},
	})

	agent := &model.AiAgent{Id: uuid.New(), Name: "Release Captain"}
	poster := func(key string) (*postCommentStatusPoster, Surface) {
		t.Helper()
		s := Surface{Kind: SurfaceChannelPost, ChannelID: channel.String(), PostID: posts[key].String()}
		p, ok := newStatusPoster(ctx, agent, s).(*postCommentStatusPoster)
		if !ok || p == nil {
			t.Fatal("no status poster for a channel post")
		}
		return p, s
	}

	// The Inbox badge, as GET /activity/unread counts it.
	unread := func(name string) int {
		t.Helper()
		n, err := activityBusiness.UnreadActivityCount(ctx, people[name], uids[name])
		if err != nil {
			t.Fatalf("%s's badge: %v", name, err)
		}
		return int(n)
	}
	// The live "replied to your message" items sent to name about the post.
	told := func(name, post string) int {
		n := 0
		for _, raw := range mq.Messages(helpers.GetMqttTopicForUserActivity(people[name].String()), int8(mqttStruct.MESSAGE_ACTIVITY)) {
			var m struct {
				Activity struct {
					Type string `json:"activity_type"`
				} `json:"activity"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Activity.Type == mqttStruct.MESSAGE_ACTIVITY_COMMENT && strings.Contains(string(raw), post) {
				n++
			}
		}
		return n
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
	// The reply emails waiting for name, by their words.
	emails := func(name string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT text_body FROM notification_email_queue WHERE event_type = 'post.comment' AND user_id = $1 ORDER BY created_at`,
			people[name])
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var bodies []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, b)
		}
		return bodies
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	settle := func() {
		t.Helper()
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		time.Sleep(500 * time.Millisecond)
	}

	const ack = "On it — I'll work on this and post back here when I'm done."

	t.Run("a slow run's ack tells nobody, and its answer is told once", func(t *testing.T) {
		post := posts["slow"].String()
		p, surface := poster("slow")
		badge := unread("bo")
		p.Ack(ctx, ack)
		p.Progress(ctx, []string{"search"})
		settle()
		if n := told("bo", post) + pushed("bo", post) + len(emails("bo")); n != 0 {
			t.Errorf("bo was told %d times of the run's \"On it\"; want 0 until it answers", n)
		}
		// The ack is a reply on bo's post, and the badge counts the replies on
		// a person's posts from the graph, told or not: it is one step now,
		// and the answer written over it is the same step.
		if got := unread("bo"); got != badge+1 {
			t.Errorf("bo's badge reads %d after the ack; want %d: the reply on their post, once", got, badge+1)
		}

		answer := "The numbers are in the sheet, row 4."
		p.Set(ctx, answer)
		notifyRunMoment(ctx, agent, surface, p, people["bo"].String(), NoticeAgentReplied, answer)
		wait("bo's live item for the answer", func() bool { return told("bo", post) > 0 })
		wait("bo's push for the answer", func() bool { return pushed("bo", post) > 0 })
		settle()
		if n := told("bo", post); n != 1 {
			t.Errorf("bo was sent %d live items for the run's answer; want 1", n)
		}
		if n := pushed("bo", post); n != 1 {
			t.Errorf("bo was pushed %d times for the run's answer; want 1", n)
		}
		if got := emails("bo"); len(got) != 1 || !strings.Contains(got[0], "row 4") || strings.Contains(got[0], "On it") {
			t.Errorf("bo's reply emails are %q; want one, with the answer's words", got)
		}
		if got := unread("bo"); got != badge+1 {
			t.Errorf("bo, who wrote the post and asked, has a badge of %d; want %d: one answer is one step", got, badge+1)
		}
		if n := told("zed", post) + pushed("zed", post); n != 0 {
			t.Errorf("zed, in the channel but not the thread, was told %d times; want 0", n)
		}
	})

	t.Run("a fast run's answer is told once", func(t *testing.T) {
		post := posts["fast"].String()
		p, surface := poster("fast")
		badge := unread("bo")
		before := len(emails("bo"))
		answer := "It's 42, in the sheet."
		p.Set(ctx, answer)
		notifyRunMoment(ctx, agent, surface, p, people["bo"].String(), NoticeAgentReplied, answer)
		wait("bo's live item for the answer", func() bool { return told("bo", post) > 0 })
		settle()
		if n := told("bo", post); n != 1 {
			t.Errorf("bo was sent %d live items for the answer; want 1", n)
		}
		if n := pushed("bo", post); n != 1 {
			t.Errorf("bo was pushed %d times for the answer; want 1", n)
		}
		if n := len(emails("bo")) - before; n != 1 {
			t.Errorf("bo has %d new reply emails; want 1", n)
		}
		if got := unread("bo"); got != badge+1 {
			t.Errorf("bo's badge reads %d; want %d: one answer is one step", got, badge+1)
		}
	})

	t.Run("a triggerer the thread doesn't put the answer in front of is still noticed once", func(t *testing.T) {
		post := posts["zeds"].String()
		p, surface := poster("zeds")
		boBadge, zedBadge := unread("bo"), unread("zed")
		answer := "Checked: the launch numbers match."
		p.Set(ctx, answer)
		notifyRunMoment(ctx, agent, surface, p, people["zed"].String(), NoticeAgentReplied, answer)
		wait("zed's push for the answer", func() bool { return pushed("zed", post) > 0 })
		settle()
		if got := unread("zed"); got != zedBadge+1 {
			t.Errorf("zed, who asked in bo's thread, has a badge of %d; want %d: the run's notice, once", got, zedBadge+1)
		}
		if got := unread("bo"); got != boBadge+1 {
			t.Errorf("bo, whose post it is, has a badge of %d; want %d", got, boBadge+1)
		}
		if n := pushed("zed", post); n != 1 {
			t.Errorf("zed was pushed %d times for the answer; want 1", n)
		}
	})
}
