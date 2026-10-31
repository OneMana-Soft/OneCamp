//go:build integration
// +build integration

package integration_test

// A reaction in a direct conversation is pushed to whoever wrote the message,
// against real Postgres, Redis and Dgraph. It was pushed to the devices of
// whoever reacted, and the author heard nothing. Now it reaches the author, as
// their setting for the conversation allows ("Nothing" hears nothing), by
// the one way a push goes out (notificationBusiness.Deliver): the desktop app
// hears of it too, and focus time holds it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADirectMessageReactionReachesItsAuthor -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADirectMessageReactionReachesItsAuthor(t *testing.T) {
	ctx := context.Background()
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

	people := map[string]uuid.UUID{}
	for _, name := range []string{"sam", "alex", "dana"} {
		id := uuid.New()
		people[name] = id
		qs := []string{`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`}
		if name != "dana" { // dana uses only the desktop app: no device
			qs = append(qs, `INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`)
		}
		for _, q := range qs {
			if _, err := env.PG.Exec(q, id, name); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
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
	send := func(name string, handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Data
	}
	// settled waits until every push and notice handed off has gone, or never will.
	settled := func() {
		t.Helper()
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
	}
	// pushed answers whose devices the reaction of kind reached.
	pushed := func(kind string, want bool) []string {
		t.Helper()
		var to []string
		find := func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind {
					to = slices.Clone(p.tokens)
					return true
				}
			}
			return false
		}
		if want {
			waitFor(t, "a "+kind+" push", find)
		} else {
			settled()
			find()
		}
		mu.Lock()
		pushes = nil
		mu.Unlock()
		return to
	}

	// noticed answers whether name's desktop app was told of a reaction of kind.
	noticed := func(name, kind string) bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		topic := helpers.GetMqttTopicForUserActivity(people[name].String())
		for _, m := range broker.got {
			var msg struct {
				Type int8                         `json:"type"`
				Data mqttStruct.MqttDesktopNotice `json:"data"`
			}
			if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE &&
				msg.Data.Data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind {
				return true
			}
		}
		return false
	}

	lunch := send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Lunch?</p>"})["uuid"].(string)

	t.Run("a reaction to a message reaches whoever wrote it", func(t *testing.T) {
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: lunch, EmojiReactionUuid: "thumbs_up"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, true); !slices.Equal(to, []string{"tok-sam"}) {
			t.Errorf("pushed to %v, want sam's phone, not alex's", to)
		}
	})

	t.Run("a reaction reaches its author's desktop app, with no device as well", func(t *testing.T) {
		hello := send("dana", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Hello from the app</p>"})["uuid"].(string)
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: hello, EmojiReactionUuid: "wave"})
		settled()
		if !noticed("dana", firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION) {
			t.Error("dana's desktop app wasn't told of alex's reaction")
		}
		if noticed("alex", firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION) {
			t.Error("alex, who reacted, was told of it")
		}
	})

	t.Run("a reaction to a reply reaches whoever wrote the reply", func(t *testing.T) {
		reply := send("sam", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: lunch, HTMLText: "<p>12:30?</p>"})["comment_id"].(string)
		send("alex", chatController.CreateOrUpdateChatCommentReaction, chatAdapter.InputUpdateReactionForCommentInChat{Uuid: reply, EmojiReactionUuid: "thumbs_up"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT_REACTION, true); !slices.Equal(to, []string{"tok-sam"}) {
			t.Errorf("pushed to %v, want sam's phone, not alex's", to)
		}
	})

	t.Run("an author who hears nothing from the conversation isn't pushed", func(t *testing.T) {
		grp := helpers.GetGroupingId(people["sam"].String(), people["alex"].String())
		if _, err := env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'block' WHERE user_id = $1 AND grp_id = $2`, people["sam"], grp); err != nil {
			t.Fatal(err)
		}
		// A message alex hasn't reacted to: an author hears of someone's
		// reactions once, so a second one to lunch would say nothing either way.
		coffee := send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Coffee?</p>"})["uuid"].(string)
		pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false) // clears what came before
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: coffee, EmojiReactionUuid: "heart"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false); len(to) != 0 {
			t.Errorf("pushed to %v while sam hears nothing from it", to)
		}
	})

	t.Run("an author in focus time is told nothing of it", func(t *testing.T) {
		// sam hears everything from alex again, so focus time alone holds it.
		grp := helpers.GetGroupingId(people["sam"].String(), people["alex"].String())
		if _, err := env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'all' WHERE user_id = $1 AND grp_id = $2`, people["sam"], grp); err != nil {
			t.Fatal(err)
		}
		heldEvent(t, env.PG, people["sam"], "Deep work", time.Now().Add(-10*time.Minute), time.Now().Add(50*time.Minute), true)
		tea := send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Tea?</p>"})["uuid"].(string)
		pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false) // clears what came before
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: tea, EmojiReactionUuid: "tea"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false); len(to) != 0 {
			t.Errorf("pushed to %v while sam is in focus time", to)
		}
	})
}
