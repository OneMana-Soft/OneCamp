//go:build integration
// +build integration

package integration_test

// Who hears of a reply in a private channel's thread, against real Postgres
// and Dgraph: the post's author, those who replied before and those it
// mentions, who are in the channel now, each by their setting for it ("all"
// and "mention": all of them; "block": the author and those mentioned). Push,
// email and the Activity feed agree.
//
// On the default setting ("all") the author and those mentioned were never
// pushed, nor on "mention" a thread one had replied in, while email went to
// everyone named, unchecked: someone mentioned from outside the channel, or
// removed from it since, was sent the reply's text, and found it in their
// Activity feed.
//
// Pushes are watched where every path sends them, and emails are read from the
// queue they wait in, so this runs without Firebase or an email provider.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAThreadReplyReachesTheChannelOnly -v

import (
	"bytes"
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

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAThreadReplyReachesTheChannelOnly(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
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
	// #launch-pricing is private. bo wrote the post; ada, dee and gus replied
	// to it before, and so did fay, who has since been removed from the
	// channel. maya replies now, mentioning cy, eve, fin and jonas. jonas
	// isn't in the channel; zed is, and has no part in the thread. Settings:
	// dee and fin hear of mentions and threads they're in, eve and gus of
	// nothing, the rest of everything.
	people := map[string]uuid.UUID{}
	byID := map[string]string{}
	everyone := []string{"maya", "bo", "ada", "cy", "dee", "eve", "fin", "gus", "zed", "fay", "jonas"}
	for _, name := range everyone {
		id := uuid.New()
		people[name] = id
		byID[id.String()] = name
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@acme.test", name, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "tok-"+name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, id, "unsub-"+name)
	}
	channel, post := uuid.New(), uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch-pricing', true)`, channel)
	for name, setting := range map[string]string{"maya": "all", "bo": "all", "ada": "all", "cy": "all", "dee": "mention",
		"fin": "mention", "eve": "block", "gus": "block", "zed": "all"} {
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`,
			people[name], channel, setting)
	}
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, channel, people["bo"])
	zero := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, name := range everyone {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User",
			"user_uuid": people[name].String(), "user_name": name})
	}
	reply := func(name, text string) map[string]any {
		return map[string]any{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": text,
			"comment_deleted_at": zero, "comment_by": map[string]any{"uid": "_:" + name},
			"comment_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}
	}
	var members []map[string]any
	for _, name := range []string{"maya", "bo", "ada", "cy", "dee", "eve", "fin", "gus", "zed"} {
		members = append(members, map[string]any{"uid": "_:" + name})
	}
	uids := dg.Mutate(t, append(nodes, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(),
		"ch_name": "launch-pricing", "ch_private": true, "ch_deleted_at": zero, "ch_members": members,
		"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": post.String(),
			"post_text": "<p>Launch prices for review</p>", "post_deleted_at": zero,
			"post_created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			"post_by":         map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"},
			"post_comments": []map[string]any{reply("ada", "<p>Looks right</p>"), reply("dee", "<p>One question</p>"),
				reply("gus", "<p>Fine by me</p>"), reply("fay", "<p>Before I left</p>")}}}}))

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
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, people[name], uids[name], name)
	}
	html := "<p>" + mention("cy") + " " + mention("eve") + " " + mention("fin") + " " + mention("jonas") + " can you check these numbers?</p>"
	raw, _ := json.Marshal(postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post.String(), HTMLText: html})
	rec := httptest.NewRecorder()
	postController.CreateCommentInPost(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as("maya")))
	if rec.Code != http.StatusOK {
		t.Fatalf("maya's reply: %d %s", rec.Code, rec.Body.String())
	}

	want := []string{"ada", "bo", "cy", "dee", "eve", "fin"}

	t.Run("push", func(t *testing.T) {
		var got []string
		waitFor(t, "the reply's push", func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT {
					got = nil
					for _, tok := range p.tokens {
						got = append(got, strings.TrimPrefix(tok, "tok-"))
					}
					return true
				}
			}
			return false
		})
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("pushed to %v, want %v: bo wrote the post, ada and dee replied, cy, eve and fin were mentioned; not gus (replied, hears nothing), zed (not in the thread), jonas (not in the channel), fay (removed)", got, want)
		}
	})

	t.Run("email", func(t *testing.T) {
		emailed := func() []string {
			rows, err := env.PG.Query(`SELECT user_id::text FROM notification_email_queue WHERE event_type = 'post.comment'`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				out = append(out, byID[id])
			}
			slices.Sort(out)
			return out
		}
		waitFor(t, "the reply's emails", func() bool { return len(emailed()) >= len(want) })
		time.Sleep(500 * time.Millisecond) // any to someone else would be in by now
		if got := emailed(); !slices.Equal(got, want) {
			t.Errorf("emailed %v, want %v", got, want)
		}
	})

	t.Run("activity", func(t *testing.T) {
		feed := func(name string) string {
			t.Helper()
			page, err := activityBusiness.GetUnifiedActivity(ctx, people[name], uids[name], time.Now().Add(time.Minute), 20)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(page.Activities)
			return string(b)
		}
		if got := feed("cy"); !strings.Contains(got, "check these numbers") {
			t.Errorf("cy, mentioned and in the channel, has no mention: %s", got)
		}
		if got := feed("jonas"); strings.Contains(got, "check these numbers") {
			t.Errorf("jonas, mentioned from outside the channel, reads the reply in his feed: %s", got)
		}
	})
}
