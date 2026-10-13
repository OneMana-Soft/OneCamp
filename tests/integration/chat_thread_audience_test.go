//go:build integration
// +build integration

package integration_test

// Who hears of a reply in a group thread, against real Postgres, Redis and
// Dgraph: the message's author, those who replied before and those it
// mentions, who are in the group, each by their setting for it. Push, the
// desktop notice, the live Activity items and email all reach that one
// audience (BE-11-G-1).
//
// Push and the desktop notice went to everyone in the group on "all", plus
// those mentioned on "mention"; Activity and email went to the author and
// those mentioned. A channel thread already told one audience all four ways.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGroupThreadReplyIsToldToOneAudience -v

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

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAGroupThreadReplyIsToldToOneAudience(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	broker := &noticeBroker{}
	mqttInit.MqttClient = broker
	t.Cleanup(func() { mqttInit.MqttClient = nil })
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
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
	// ada, bo, cy, dee and zed are in the group; eve isn't.
	everyone := []string{"ada", "bo", "cy", "dee", "zed", "eve"}
	people := map[string]uuid.UUID{}
	byID := map[string]string{}
	uids := map[string]string{}
	for _, name := range everyone {
		id := uuid.New()
		people[name], byID[id.String()] = id, name
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`, id, name)
		got := dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
		uids[name] = got["u"]
	}
	info := func(name string) userModels.UserInfo {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
	}
	all := map[string]userModels.UserInfo{}
	for _, name := range everyone {
		all[name] = info(name)
	}
	send := func(name string, handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(
			context.WithValue(ctx, helpers.UserInfoContextKey, info(name))))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Data
	}
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, people[name], uids[name], name)
	}

	// Ada starts the group and writes the message the thread is under.
	var members []string
	for _, n := range []string{"ada", "bo", "cy", "dee", "zed"} {
		members = append(members, uids[n])
	}
	message, _ := send("ada", chatController.CreateGroupChat, chatAdapter.ChatInfo{Participants: members, TextHtml: "<p>Offsite dates?</p>"})["uuid"].(string)
	if message == "" {
		t.Fatal("ada's message has no id")
	}
	var grp string
	if err := env.PG.QueryRow(`SELECT grp_id FROM users_chat_notification WHERE user_id = $1 LIMIT 1`, people["zed"]).Scan(&grp); err != nil {
		t.Fatalf("the group's settings: %v", err)
	}
	// dee hears nothing of the group unless addressed.
	exec(`UPDATE users_chat_notification SET notification_type = 'block' WHERE user_id = $1 AND grp_id = $2`, people["dee"], grp)

	// cy replied first; now bo replies, mentioning dee and eve.
	send("cy", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: message, HTMLText: "<p>I'm in</p>"})
	time.Sleep(time.Second) // cy's notices are out of the way
	reply, _ := send("bo", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{
		ChatUuid: message, HTMLText: "<p>" + mention("dee") + " " + mention("eve") + " the 14th works</p>"})["comment_id"].(string)
	if reply == "" {
		t.Fatal("bo's reply has no id")
	}

	want := []string{"ada", "cy", "dee"}
	why := "ada wrote the message, cy replied, dee was mentioned (on Nothing, but addressed); never bo (wrote it), zed (in the group, not the thread) or eve (not in the group)"

	t.Run("push", func(t *testing.T) {
		var got []string
		waitFor(t, "the reply's push", func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT &&
					strings.Contains(p.data[firebaseInit.FIREBASE_PUSH_DATA_BODY], "14th") {
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
			t.Errorf("pushed to %v, want %v: %s", got, want, why)
		}
	})

	t.Run("desktop notice", func(t *testing.T) {
		id := "chat_comment:" + message + ":" + reply
		waitFor(t, "the reply's notices", func() bool { return len(broker.notices(id, all)) >= len(want) })
		if got := broker.notices(id, all); !slices.Equal(got, want) {
			t.Errorf("noticed by %v, want %v: %s", got, want, why)
		}
	})

	t.Run("email", func(t *testing.T) {
		var got []string
		waitFor(t, "the reply's emails", func() bool {
			rows, err := env.PG.Query(`SELECT user_id::text FROM notification_email_queue WHERE strpos(dedup_key, $1) > 0`, reply)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got = nil
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				got = append(got, byID[id])
			}
			return len(got) >= len(want)
		})
		time.Sleep(500 * time.Millisecond)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("emailed %v, want %v: %s", got, want, why)
		}
	})

	t.Run("activity", func(t *testing.T) {
		// Each one's live items for this reply, by kind.
		items := func() map[string]string {
			broker.mu.Lock()
			defer broker.mu.Unlock()
			out := map[string]string{}
			for name, p := range all {
				topic := helpers.GetMqttTopicForUserActivity(p.UserDgraphInfo.Uuid)
				for _, m := range broker.got {
					s := string(m.payload)
					if m.topic != topic || !strings.Contains(s, reply) {
						continue
					}
					for _, kind := range []string{"COMMENT", "MENTION"} {
						if strings.Contains(s, `"activity_type":"`+kind+`"`) {
							out[name] = kind
						}
					}
				}
			}
			return out
		}
		waitFor(t, "the reply's Activity items", func() bool { return len(items()) >= 2 })
		time.Sleep(500 * time.Millisecond)
		if got, want := items(), map[string]string{"ada": "COMMENT", "dee": "MENTION"}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Activity items %v, want %v: the author's reply item and the mention item, from the same audience", got, want)
		}
	})
}
