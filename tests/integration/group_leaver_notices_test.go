//go:build integration
// +build integration

package integration_test

// Someone who leaves a group conversation is told nothing more of it, against
// real Postgres, Redis and Dgraph: not its new messages (push, desktop notice,
// email), not a reply in the thread of a message they wrote there, and not a
// reaction to one (push or Activity). Its people are told as before.
//
// Who hears of a direct or group message was read from its people's settings
// (users_chat_notification) alone. Leaving a group kept the leaver's setting,
// and the start-up repair gave a setting back to anyone who had read the
// conversation (last_seen_chat), so whoever left went on hearing it. Installs
// still hold settings kept that way: one is told nothing either, and the
// start-up repair takes it away.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSomeoneWhoLeftAGroupIsToldNothingMoreOfIt -v

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
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSomeoneWhoLeftAGroupIsToldNothingMoreOfIt(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	dg := integration.SetupDgraph(t)
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
	people := map[string]uuid.UUID{}
	byID := map[string]string{}
	uids := map[string]string{}
	for _, name := range []string{"ada", "bo", "cy", "dee"} {
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
	as := func(name string) context.Context {
		t.Helper()
		return context.WithValue(ctx, helpers.UserInfoContextKey, info(name))
	}
	// send runs a route as name and answers the id its answer names by key.
	send := func(name string, handler http.HandlerFunc, body any, key string) string {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		id, _ := out.Data[key].(string)
		if rec.Code != http.StatusOK || (key != "" && id == "") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		return id
	}
	settle := func() {
		t.Helper()
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
	}
	// pushedTo is whose phones the pushes that match picks reached.
	pushedTo := func(match func(map[string]string) bool) []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, p := range pushes {
			if !match(p.data) {
				continue
			}
			for _, tok := range p.tokens {
				if n := strings.TrimPrefix(tok, "tok-"); !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	// emailed is who was emailed of the message or reply id.
	emailed := func(id string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT DISTINCT user_id::text FROM notification_email_queue WHERE strpos(dedup_key, $1) > 0`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var u string
			_ = rows.Scan(&u)
			out = append(out, byID[u])
		}
		slices.Sort(out)
		return out
	}
	hasSetting := func(name, grp string) bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM users_chat_notification WHERE grp_id = $1 AND user_id = $2`, grp, people[name]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, people[name], uids[name], name)
	}
	// mark is where the live messages published so far end; reactionItem is
	// whether name's Activity was sent a reaction item after it.
	mark := func() int {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return len(broker.got)
	}
	reactionItem := func(name string, since int) bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		topic := helpers.GetMqttTopicForUserActivity(people[name].String())
		for _, m := range broker.got[since:] {
			if m.topic == topic && strings.Contains(string(m.payload), `"activity_type":"`+mqttStruct.MESSAGE_ACTIVITY_REACTION+`"`) {
				return true
			}
		}
		return false
	}

	// Ada starts a group of four (people are named by their graph uids). Cy
	// writes in it, and Ada answers in the thread of Cy's message.
	send("ada", chatController.CreateGroupChat, chatAdapter.ChatInfo{
		Participants: []string{uids["ada"], uids["bo"], uids["cy"], uids["dee"]}, TextHtml: "<p>Kickoff</p>"}, "")
	grp, err := helpers.GenerateGroupID([]string{people["ada"].String(), people["bo"].String(), people["cy"].String(), people["dee"].String()})
	if err != nil {
		t.Fatal(err)
	}
	cyMsg := send("cy", chatController.CreateGroupChat, chatAdapter.ChatInfo{GrpUuid: grp, TextHtml: "<p>Draft of the launch post</p>"}, "uuid")
	adaMsg := send("ada", chatController.CreateGroupChat, chatAdapter.ChatInfo{GrpUuid: grp, TextHtml: "<p>Venue booked</p>"}, "uuid")
	send("ada", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: cyMsg, HTMLText: "<p>Looks good</p>"}, "comment_id")
	// Cy has read the group, as anyone in it has: the row the start-up repair
	// gives settings from.
	exec(`INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES ($1, $2, NOW()) ON CONFLICT (user_id, grp_id) DO NOTHING`, people["cy"], grp)
	if !hasSetting("cy", grp) {
		t.Fatal("cy had no setting for the group before leaving it")
	}

	// Cy leaves it, as the app's Leave does.
	cy := info("cy")
	if err := conversationBusiness.Leave(as("cy"), &cy, conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: grp}); err != nil {
		t.Fatalf("cy leaves the group: %v", err)
	}
	settle()
	mu.Lock()
	pushes = nil
	mu.Unlock()

	t.Run("leaving takes away their setting for the group", func(t *testing.T) {
		if hasSetting("cy", grp) {
			t.Error("cy left the group and still has a setting for it")
		}
	})

	// Installs from before leaving took it away still hold a leaver's setting:
	// cy keeps one from here on, and is told nothing all the same.
	exec(`INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, 'all') ON CONFLICT (user_id, grp_id) DO NOTHING`, people["cy"], grp)

	t.Run("a new message reaches the group's people and not them", func(t *testing.T) {
		msg := send("bo", chatController.CreateGroupChat, chatAdapter.ChatInfo{GrpUuid: grp,
			TextHtml: "<p>" + mention("ada") + " " + mention("dee") + " the plan for Friday is up</p>"}, "uuid")
		isMsg := func(d map[string]string) bool {
			return d[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT &&
				d[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == msg
		}
		waitFor(t, "the message's pushes to ada and dee", func() bool { return len(pushedTo(isMsg)) >= 2 })
		waitFor(t, "the message's emails to ada and dee", func() bool { return len(emailed(msg)) >= 2 })
		settle()
		want := []string{"ada", "dee"}
		if got := pushedTo(isMsg); !slices.Equal(got, want) {
			t.Errorf("pushed to %v, want %v: cy left the group, bo wrote it", got, want)
		}
		if got := emailed(msg); !slices.Equal(got, want) {
			t.Errorf("emailed %v, want %v: cy left the group, bo wrote it", got, want)
		}
	})

	t.Run("a reply in the thread of their message isn't told to them", func(t *testing.T) {
		reply := send("bo", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: cyMsg, HTMLText: "<p>Published the draft</p>"}, "comment_id")
		isReply := func(d map[string]string) bool {
			return d[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT &&
				strings.Contains(d[firebaseInit.FIREBASE_PUSH_DATA_BODY], "Published the draft")
		}
		waitFor(t, "the reply's push to ada", func() bool { return len(pushedTo(isReply)) >= 1 })
		settle()
		want := []string{"ada"}
		if got := pushedTo(isReply); !slices.Equal(got, want) {
			t.Errorf("pushed to %v, want %v: ada replied in the thread; cy wrote its message but left the group", got, want)
		}
		if got := emailed(reply); slices.Contains(got, "cy") {
			t.Errorf("emailed %v: cy left the group", got)
		}
	})

	t.Run("a reaction to their message isn't told to them", func(t *testing.T) {
		since := mark()
		send("bo", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: adaMsg, EmojiReactionUuid: "rocket"}, "")
		send("bo", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: cyMsg, EmojiReactionUuid: "tada"}, "")
		reactionTo := func(msg string) func(map[string]string) bool {
			return func(d map[string]string) bool {
				return d[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION &&
					d[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == msg
			}
		}
		waitFor(t, "the reaction's push to ada", func() bool { return len(pushedTo(reactionTo(adaMsg))) >= 1 })
		settle()
		if got := pushedTo(reactionTo(adaMsg)); !slices.Equal(got, []string{"ada"}) {
			t.Errorf("a reaction to ada's message was pushed to %v, want ada", got)
		}
		if got := pushedTo(reactionTo(cyMsg)); len(got) != 0 {
			t.Errorf("a reaction to cy's message was pushed to %v: cy left the group", got)
		}
		if !reactionItem("ada", since) {
			t.Error("ada's Activity wasn't sent the reaction to her message")
		}
		if reactionItem("cy", since) {
			t.Error("cy's Activity was sent a reaction to cy's message: cy left the group")
		}
	})

	t.Run("the start-up repair takes the setting they kept away, gives none back, and still mends a member's", func(t *testing.T) {
		exec(`DELETE FROM users_chat_notification WHERE grp_id = $1 AND user_id = $2`, grp, people["dee"])
		// The repair as every start runs it: it gives dee's setting back last.
		repair, stop := context.WithCancel(ctx)
		defer stop()
		chatNotificationBusiness.StartChatSettingsRepair(repair)
		for deadline := time.Now().Add(30 * time.Second); !hasSetting("dee", grp); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the start-up repair didn't give dee, in the group, her setting back")
			}
		}
		if hasSetting("cy", grp) {
			t.Error("after the repair, cy, who left the group, has a setting for it")
		}
		for _, name := range []string{"ada", "bo"} {
			if !hasSetting(name, grp) {
				t.Errorf("the repair took away %s's setting: %s is in the group", name, name)
			}
		}
	})
}
